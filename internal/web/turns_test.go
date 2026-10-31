package web

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"ai-trace/internal/ir"
	"ai-trace/internal/store"
)

// ---- classifyBody：各协议的消息收尾形态 → 是否新轮 ----

// anthropicBody 构造 Anthropic 形态请求体，末条消息由 content 决定。
func anthropicBody(last map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":    "test",
		"messages": []map[string]any{{"role": "user", "content": "前面的话"}, last},
	})
	return b
}

func TestClassifyBody(t *testing.T) {
	userText := anthropicBody(map[string]any{"role": "user", "content": "帮我看看这个报错"})
	toolResult := anthropicBody(map[string]any{"role": "user", "content": []map[string]any{
		{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
	}})
	mixed := anthropicBody(map[string]any{"role": "user", "content": []map[string]any{
		{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		{"type": "text", "text": "<system-reminder>上下文提醒</system-reminder>"},
	}})
	openaiTool, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "tool", "content": `{"res":1}`},
		},
	})
	openaiUser, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "再来一次"}},
	})
	responsesTool, _ := json.Marshal(map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hi"},
			{"type": "function_call_output", "call_id": "c1", "output": "42"},
		},
	})
	responsesUser, _ := json.Marshal(map[string]any{
		"input": []map[string]any{{"type": "message", "role": "user", "content": "新问题"}},
	})

	cases := []struct {
		name   string
		body   []byte
		starts bool
		text   string
	}{
		{"anthropic 纯文本收尾 → 新轮", userText, true, "帮我看看这个报错"},
		{"anthropic tool_result 收尾 → 延续", toolResult, false, ""},
		{"anthropic tool_result 混 reminder 文本 → 延续", mixed, false, ""},
		{"openai tool role 收尾 → 延续", openaiTool, false, ""},
		{"openai user 文本收尾 → 新轮", openaiUser, true, "再来一次"},
		{"responses function_call_output 收尾 → 延续", responsesTool, false, ""},
		{"responses message 收尾 → 新轮", responsesUser, true, "新问题"},
		{"豆包私有形态（无 messages）→ 兜底自成新轮", []byte(`{"content_block":[]}`), true, ""},
		{"垃圾体 → 兜底自成新轮", []byte(`not-json`), true, ""},
		{"空体 → 兜底自成新轮", nil, true, ""},
	}
	for _, c := range cases {
		starts, text := classifyBody(c.body)
		if starts != c.starts || text != c.text {
			t.Errorf("%s: classifyBody = (%v, %q), want (%v, %q)",
				c.name, starts, text, c.starts, c.text)
		}
	}
}

func TestCutText(t *testing.T) {
	if got := cutText("<user_query>导出报表</user_query>", 200); got != "导出报表" {
		t.Errorf("user_query 应被提取: %q", got)
	}
	if got := cutText("<system-reminder>一大段包装</system-reminder>真正的问题", 200); got != "真正的问题" {
		t.Errorf("system-reminder 应被剥掉: %q", got)
	}
	long := ""
	for i := 0; i < 50; i++ {
		long += "汉字"
	}
	got := cutText(long, 60)
	if len([]rune(got)) != 61 || !endsWith(got, "…") { // 60 字 + 省略号
		t.Errorf("超长应截断到 60 字加省略号: %d 字", len([]rune(got)))
	}
}

func endsWith(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

// ---- buildTurns / TurnsForDay：切轮、汇总与分组 ----

// turnStore 造一个临时账本：recs 依次 Append（body 全量由 bodies 给出，
// 可带 RequestBody/Result）。注意 Append 以写入日决定账本文件，测试记录
// 的 ID 前缀须与当天一致。
func turnStore(t *testing.T, recs []store.Record, bodies map[string]store.FullBody) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		body := bodies[r.ID]
		body.ID = r.ID
		st.Append(r, body)
	}
	return st
}

func msgBody(last map[string]any) store.FullBody {
	b, _ := json.Marshal(map[string]any{"messages": []map[string]any{last}})
	return store.FullBody{RequestBody: b}
}

func resultBody(last map[string]any, res *ir.Result) store.FullBody {
	b := msgBody(last)
	b.Result = res
	return b
}

func mkRec(id string, at time.Time, session, subagent string) store.Record {
	return store.Record{
		ID: id, Time: at, Agent: "claude-code", Session: session, Subagent: subagent,
		Protocol: "anthropic", Path: "/v1/messages", Status: 200,
		Usage: &ir.Usage{Input: 100, Output: 10},
	}
}

var base = time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

func TestBuildTurns(t *testing.T) {
	// 一条完整 ReAct 链 + 一轮新问题；最后一个 tool_call 带失败状态
	recs := []store.Record{
		mkRec("r1", at(0), "s1", ""),
		mkRec("r2", at(5), "s1", ""),
		mkRec("r3", at(9), "s1", ""),
		mkRec("r4", at(30), "s1", ""),
	}
	bodies := map[string]store.FullBody{
		"r1": msgBody(map[string]any{"role": "user", "content": "修一下构建"}),
		"r2": msgBody(map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result"}}}),
		"r3": msgBody(map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result"}}}),
		"r4": msgBody(map[string]any{"role": "user", "content": "跑测试"}),
	}
	for i := range recs {
		recs[i].ToolCalls = []ir.ToolCall{{ID: "t" + fmt.Sprint(i), Name: "Bash"}}
	}
	recs[2].Error = "上游 500"
	recs[2].Status = 500
	// 耗时：r3 结束于 +12s
	recs[2].DurationMS = 3000

	turns := buildTurns(recs, func(r store.Record) turnClass {
		starts, text := classifyBody(bodies[r.ID].RequestBody)
		return turnClass{starts, text}
	})
	if len(turns) != 2 {
		t.Fatalf("应切成 2 轮，得 %d", len(turns))
	}
	first, second := turns[0], turns[1]
	if first.ID != "r1" || first.Text != "修一下构建" || first.Calls != 3 || first.Errors != 1 {
		t.Errorf("第一轮聚合错误: %+v", first)
	}
	if first.Usage.Input != 300 || first.Usage.Output != 30 {
		t.Errorf("第一轮 usage 应汇总 3 条: %+v", first.Usage)
	}
	if len(first.Tools) != 1 || first.Tools[0] != "Bash" {
		t.Errorf("工具应去重为一个 Bash: %v", first.Tools)
	}
	wantDur := 12 * 1000 // r3 开始(+9s)+耗时3s - r1 开始(0s) = 12s
	if first.DurationMS != int64(wantDur) {
		t.Errorf("轮耗时 = %dms, want %dms", first.DurationMS, wantDur)
	}
	if second.ID != "r4" || second.Text != "跑测试" || second.Calls != 1 {
		t.Errorf("第二轮应从 r4 开新轮: %+v", second)
	}
}

func TestBuildTurnsLeadingContinuation(t *testing.T) {
	// 组内首条即 tool_result（会话跨天承接）：开隐式轮，不丢记录
	recs := []store.Record{mkRec("r1", at(0), "s1", "")}
	bodies := map[string]store.FullBody{
		"r1": msgBody(map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result"}}}),
	}
	turns := buildTurns(recs, func(r store.Record) turnClass {
		starts, text := classifyBody(bodies[r.ID].RequestBody)
		return turnClass{starts, text}
	})
	if len(turns) != 1 || turns[0].Calls != 1 || turns[0].Text != "" {
		t.Fatalf("延续首条应开隐式轮: %+v", turns)
	}
}

func TestTurnsForDay(t *testing.T) {
	// 两个会话 + 一个子代理链 + 无会话杂流量，乱序 Append
	recs := []store.Record{
		mkRec("a1", at(0), "sess-A", ""),
		mkRec("a2", at(4), "sess-A", ""),
		mkRec("sub1", at(2), "sess-A", "agent-x"),
		mkRec("sub2", at(6), "sess-A", "agent-x"),
		mkRec("b1", at(20), "sess-B", ""),
		mkRec("noise1", at(25), "", ""),
	}
	recs[5].Agent = "unknown" // MITM 误录的杂流量
	bodies := map[string]store.FullBody{
		"a1":   msgBody(map[string]any{"role": "user", "content": "会话A第一问"}),
		"a2":   msgBody(map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result"}}}),
		"sub1": msgBody(map[string]any{"role": "user", "content": "子代理任务"}),
		"sub2": msgBody(map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result"}}}),
		"b1":   msgBody(map[string]any{"role": "user", "content": "会话B第一问"}),
	}
	st := turnStore(t, recs, bodies)

	turns := TurnsForDay(st, st.Days()[0])
	// sess-A 主循环 1 轮 + 子代理 1 轮 + sess-B 1 轮 + 杂流量 1 轮 = 4
	if len(turns) != 4 {
		t.Fatalf("应有 4 轮，得 %d: %+v", len(turns), turns)
	}
	// 时间降序：noise1(25s) → b1(20s) → sub(2s起) → a1(0s)
	if turns[0].ID != "noise1" {
		t.Errorf("应按时间降序，首轮 = %s", turns[0].ID)
	}
	sub := turns[2]
	if sub.ID != "sub1" || sub.Subagent != "agent-x" || sub.Calls != 2 {
		t.Errorf("子代理应独立成轮: %+v", sub)
	}
	mainA := turns[3]
	if mainA.ID != "a1" || mainA.Calls != 2 || mainA.Subagent != "" {
		t.Errorf("sess-A 主循环应为一轮 2 次调用: %+v", mainA)
	}
	if turns[1].ID != "b1" {
		t.Errorf("sess-B 轮锚点应为 b1: %+v", turns[1])
	}

	// 过滤：agent 精确 / err / 关键词
	if got := filterTurns(turns, "claude-code", "", ""); len(got) != 3 {
		t.Errorf("按 agent 过滤应剩 3 轮（杂流量是 unknown），得 %d", len(got))
	}
	recs2 := append([]store.Record(nil), recs...)
	recs2[4].Error = "boom" // b1 失败
	recs2[4].Status = 500
	st2 := turnStore(t, recs2, bodies)
	if got := filterTurns(TurnsForDay(st2, st2.Days()[0]), "", "err", ""); len(got) != 1 || got[0].ID != "b1" {
		t.Errorf("err 过滤应只留含失败调用的轮: %+v", got)
	}
	if got := filterTurns(turns, "", "", "会话B"); len(got) != 1 || got[0].ID != "b1" {
		t.Errorf("关键词应命中会话 B: %+v", got)
	}
}

func TestTurnDetail(t *testing.T) {
	recs := []store.Record{
		mkRec("r1", at(0), "s1", ""),
		mkRec("r2", at(5), "s1", ""),
	}
	bodies := map[string]store.FullBody{
		"r1": msgBody(map[string]any{"role": "user", "content": "完整链路"}),
		"r2": resultBody(
			map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result"}}},
			&ir.Result{Text: "好的", StopReason: "end_turn"}),
	}
	st := turnStore(t, recs, bodies)

	t1, calls, ok := TurnDetail(st, "r2")
	if !ok || t1.ID != "r1" || t1.Calls != 2 {
		t.Fatalf("应找到 r1 开头的轮: ok=%v turn=%+v", ok, t1)
	}
	if len(calls) != 2 || calls[1].Record.ID != "r2" {
		t.Fatalf("calls 应含两次调用: %+v", calls)
	}
	if calls[1].Result == nil || calls[1].Result.Text != "好的" {
		t.Errorf("末次调用的 result 应带聚合结果: %+v", calls[1].Result)
	}
	if calls[0].Result != nil {
		t.Errorf("r1 无聚合结果应为 nil: %+v", calls[0].Result)
	}
	if _, _, ok := TurnDetail(st, "99999999-000000-zzz"); ok {
		t.Error("不存在的记录应返回 false")
	}
}
