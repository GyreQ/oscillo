package decode

import (
	"strings"
	"testing"

	"ai-trace/internal/ir"
)

// 样本取自真实录制的豆包 2.24.10 /chat/completion 流（"你是谁" → "我是豆包…"）。
// 关键陷阱：STREAM_CHUNK 的 tts_content（patch_object=111）与 CHUNK_DELTA 重复，
// 取了会正文翻倍。
func TestDoubaoDecoderAssemblesReply(t *testing.T) {
	events := []Frame{
		{Event: "SSE_HEARTBEAT", Data: "{}"},
		{Event: "SSE_ACK", Data: `{"query_list":[]}`},
		{Event: "FULL_MSG_NOTIFY", Data: `{"message":{"user_type":1,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"你是谁"}}}]}}`},
		{Event: "STREAM_MSG_NOTIFY", Data: `{"content":{"content_block":[{"block_type":10000,"block_id":"10525983799907842",` +
			`"content":{"text_block":{"text":"我"}},"is_finish":false,"patch_type":1}],"content_status":100}}`},
		{Event: "STREAM_CHUNK", Data: `{"message_id":"1","patch_op":[{"patch_object":1,"patch_type":1,"patch_value":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":"是"}}}]}}]}`},
		{Event: "STREAM_CHUNK", Data: `{"message_id":"1","patch_op":[{"patch_object":111,"patch_type":1,"patch_value":{"tts_content":"是"}}]}`},
		{Event: "CHUNK_DELTA", Data: `{"text":"豆包，由字节跳动"}`},
		{Event: "STREAM_CHUNK", Data: `{"message_id":"1","patch_op":[{"patch_object":111,"patch_type":1,"patch_value":{"tts_content":"豆包，由字节跳动"}}]}`},
		{Event: "CHUNK_DELTA", Data: `{"text":"独立研发的AI助手。"}`},
		{Event: "SSE_REPLY_END", Data: `{"end_type":1,"msg_finish_attr":{"brief":"我是豆包，由字节跳动独立研发的AI助手。"}}`},
	}

	d := &DoubaoDecoder{}
	col := ir.NewCollector()
	for _, f := range events {
		d.FeedFrame(f, col.OnEvent)
	}
	res := col.Seal()
	want := "我是豆包，由字节跳动独立研发的AI助手。"
	if res.Text != want {
		t.Fatalf("正文拼接错误:\n got %q\nwant %q", res.Text, want)
	}
	if res.StopReason == "" {
		t.Fatal("缺少 stop 事件")
	}
}

// 整条流没有增量时（缓存/重生成），用 SSE_REPLY_END 的 brief 兜底。
func TestDoubaoDecoderBriefFallback(t *testing.T) {
	d := &DoubaoDecoder{}
	col := ir.NewCollector()
	d.FeedFrame(Frame{Event: "SSE_HEARTBEAT", Data: "{}"}, col.OnEvent)
	d.FeedFrame(Frame{Event: "SSE_REPLY_END",
		Data: `{"end_type":1,"msg_finish_attr":{"brief":"缓存回复"}}`}, col.OnEvent)
	if got := col.Seal().Text; got != "缓存回复" {
		t.Fatalf("brief 兜底失败: got %q", got)
	}
}

func TestDoubaoDecoderProtocol(t *testing.T) {
	if got := (&DoubaoDecoder{}).Protocol(); got != ProtoDoubao {
		t.Fatalf("protocol = %q", got)
	}
}

// 完整 SSE 原文过 Scanner 分帧后解码（模拟网关真实路径）。
func TestDoubaoDecoderViaScanner(t *testing.T) {
	raw := "event: STREAM_MSG_NOTIFY\n" +
		`data: {"content":{"content_block":[{"content":{"text_block":{"text":"你好"}}}]}}` + "\n\n" +
		"event: CHUNK_DELTA\ndata: {\"text\":\"世界\"}\n\n" +
		"event: SSE_REPLY_END\ndata: {\"end_type\":1,\"msg_finish_attr\":{\"brief\":\"x\"}}\n\n"
	sc := &Scanner{}
	d := &DoubaoDecoder{}
	col := ir.NewCollector()
	for _, f := range sc.Write([]byte(raw)) {
		d.FeedFrame(f, col.OnEvent)
	}
	for _, f := range sc.Flush() {
		d.FeedFrame(f, col.OnEvent)
	}
	if got := col.Seal().Text; !strings.Contains(got, "你好世界") {
		t.Fatalf("Scanner 路径拼接失败: %q", got)
	}
}
