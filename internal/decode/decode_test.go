package decode

import (
	"strings"
	"testing"

	"ai-trace/internal/ir"
)

// feedSSE 把一段 SSE 文本喂给解码器，返回聚合结果。
func feedSSE(t *testing.T, d Decoder, sse string) ir.Result {
	t.Helper()
	scan := &Scanner{}
	coll := ir.NewCollector()
	for _, f := range scan.Write([]byte(sse)) {
		d.FeedFrame(f, coll.OnEvent)
	}
	for _, f := range scan.Flush() {
		d.FeedFrame(f, coll.OnEvent)
	}
	return coll.Seal()
}

// TestScannerSSEFrames SSE 分帧：事件名、多行 data、注释行、跨块写入。
func TestScannerSSEFrames(t *testing.T) {
	scan := &Scanner{}
	// 跨块写入：故意在行中间断开
	a := scan.Write([]byte("event: message_start\nda"))
	b := scan.Write([]byte("ta: {\"x\":1}\n\n: keepalive\n\n"))
	all := append(a, b...)
	if len(all) != 1 {
		t.Fatalf("期望 1 帧，得到 %d", len(all))
	}
	if all[0].Event != "message_start" || all[0].Data != "{\"x\":1}\n" {
		t.Fatalf("帧内容不对: %+v", all[0])
	}
	// Flush 冲刷没有结尾空行的残帧
	scan.Write([]byte("event: tail\ndata: 99"))
	if f := scan.Flush(); len(f) != 1 || f[0].Data != "99\n" {
		t.Fatalf("Flush 结果不对: %+v", f)
	}
}

// TestAnthropicStream 完整 Anthropic 流：文本 + thinking + tool_use 增量 + usage。
func TestAnthropicStream(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	res := feedSSE(t, &AnthropicDecoder{}, sse)
	if res.Text != "你好" {
		t.Errorf("文本不对: %q", res.Text)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "Bash" {
		t.Fatalf("工具调用不对: %+v", res.ToolCalls)
	}
	if string(res.ToolCalls[0].Args) != `{"cmd":"ls"}` {
		t.Errorf("工具参数拼接不对: %s", res.ToolCalls[0].Args)
	}
	if res.StopReason != "tool_use" {
		t.Errorf("stop 不对: %q", res.StopReason)
	}
	if res.Usage == nil || res.Usage.Input != 100 || res.Usage.CacheRead != 50 || res.Usage.Output != 42 {
		t.Errorf("usage 不对: %+v", res.Usage)
	}
}

// TestAnthropicNonStream 非流式 Anthropic 响应。
func TestAnthropicNonStream(t *testing.T) {
	body := `{"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/x"}}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	d := &AnthropicDecoder{}
	coll := ir.NewCollector()
	d.FeedBody([]byte(body), coll.OnEvent)
	res := coll.Seal()
	if res.Text != "hi" || len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "Read" {
		t.Fatalf("聚合不对: %+v", res)
	}
	if string(res.ToolCalls[0].Args) != `{"path":"/x"}` {
		t.Errorf("参数不对: %s", res.ToolCalls[0].Args)
	}
}

// TestOpenAIChatStream OpenAI chat 流：文本 + reasoning + 工具调用增量 + usage。
func TestOpenAIChatStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"想想"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"答"}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":200,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":80}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	res := feedSSE(t, &OpenAIChatDecoder{}, sse)
	if res.Text != "答" || res.Thinking != "想想" {
		t.Errorf("文本不对: %q %q", res.Text, res.Thinking)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_1" ||
		string(res.ToolCalls[0].Args) != `{"city":"北京"}` {
		t.Fatalf("工具调用不对: %+v", res.ToolCalls)
	}
	if res.StopReason != "tool_use" {
		t.Errorf("stop 不对: %q", res.StopReason)
	}
	// prompt_tokens=200 含 cached=80，Input 应为 120、CacheRead 80
	if res.Usage == nil || res.Usage.Input != 120 || res.Usage.CacheRead != 80 || res.Usage.Output != 30 {
		t.Errorf("usage 不对: %+v", res.Usage)
	}
}

// TestOpenAIResponsesStream OpenAI responses 流（Codex 协议）：
// function_call 的 item_id(fc_xxx) 要映射回 call_id(call_xxx)。
func TestOpenAIResponsesStream(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_01","call_id":"call_01","name":"shell"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_01","delta":"{\"command\":"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_01","delta":"\"pwd\"}"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":500,"output_tokens":60,"input_tokens_details":{"cached_tokens":100},"output_tokens_details":{"reasoning_tokens":40}}}}`,
		``,
	}, "\n")

	res := feedSSE(t, &OpenAIResponsesDecoder{}, sse)
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_01" || res.ToolCalls[0].Name != "shell" {
		t.Fatalf("工具调用不对: %+v", res.ToolCalls)
	}
	if string(res.ToolCalls[0].Args) != `{"command":"pwd"}` {
		t.Errorf("参数不对: %s", res.ToolCalls[0].Args)
	}
	if res.Usage == nil || res.Usage.Input != 400 || res.Usage.CacheRead != 100 ||
		res.Usage.Output != 60 || res.Usage.Reasoning != 40 {
		t.Errorf("usage 不对: %+v", res.Usage)
	}
}

// TestScannerChunkedNoDup 回归测试：分块写入（块边界在行中间断开）时
// 帧不能被重复产出（曾因 Write 返回累积帧导致 toolCalls 翻倍）。
func TestScannerChunkedNoDup(t *testing.T) {
	sse := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"B"}}` + "\n\n" +
		"event: message_stop\ndata: {}\n\n"
	// 按 7 字节一块喂
	scan := &Scanner{}
	total := 0
	seen := map[string]int{}
	for i := 0; i < len(sse); i += 7 {
		for _, f := range scan.Write([]byte(sse[i:min(i+7, len(sse))])) {
			total++
			seen[f.Data]++
		}
	}
	for _, f := range scan.Flush() {
		total++
		seen[f.Data]++
	}
	if total != 2 {
		t.Fatalf("期望 2 帧，得到 %d: %v", total, seen)
	}
	for d, n := range seen {
		if n != 1 {
			t.Fatalf("帧重复产出 %d 次: %.60s", n, d)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestUsageMerge usage 多次报告只取非零末值。
func TestUsageMerge(t *testing.T) {
	var u ir.Usage
	u.Merge(ir.Usage{Input: 10})
	u.Merge(ir.Usage{Input: 0, Output: 5}) // 中途报告 input=0 不覆盖
	if u.Input != 10 || u.Output != 5 {
		t.Fatalf("merge 不对: %+v", u)
	}
	if u.Prompt() != 10 || u.Total() != 15 {
		t.Fatalf("口径不对: %d %d", u.Prompt(), u.Total())
	}
}
