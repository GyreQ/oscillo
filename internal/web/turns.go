// turns.go 用户请求轮次：把同会话的多次模型调用聚合成"一轮用户请求"。
//
// 视角翻转：账本里一行 = 一次模型调用（模型侧）；用户侧的一轮 = 一次用户
// 输入触发的 N 次调用。切轮规则看模型请求的消息列表末尾——是工具结果
// （OpenAI 的 role:"tool"、Anthropic 的 tool_result 块、Responses API 的
// function_call_output）就归入当前轮继续，是纯用户文本就开启新轮；
// 解析不了的请求体（豆包 content_block 等私有形态）安全兜底为自成一轮。
package web

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"ai-trace/internal/ir"
	"ai-trace/internal/store"
)

// UserTurn 一轮用户请求：一次用户输入触发的 N 次模型调用。
type UserTurn struct {
	ID         string   `json:"id"` // 锚点 = 轮内首条记录 id
	Session    string   `json:"session,omitempty"`
	Subagent   string   `json:"subagent,omitempty"`
	Agent      string   `json:"agent"`
	Time       string   `json:"time"`              // 首条调用开始（RFC3339）
	EndTime    string   `json:"endTime,omitempty"` // 末条调用开始
	DurationMS int64    `json:"durationMs"`        // 末条结束 - 首条开始
	Text       string   `json:"text,omitempty"`    // 用户输入（清洗截断后）
	Calls      int      `json:"calls"`             // 模型调用次数
	Errors     int      `json:"errors"`            // 失败调用数
	Usage      ir.Usage `json:"usage"`             // 全轮汇总
	Tools      []string `json:"tools,omitempty"`   // 触发过的工具（去重）

	records []store.Record // 轮内记录（时间序）；内部用，不进 JSON
}

// turnClass 一条记录的切轮分类结果（body 落盘后不可变，可放心缓存）。
type turnClass struct {
	starts bool   // 是否开启新一轮
	text   string // 新轮的用户输入文本（已清洗截断）
}

// bodyClassCache 进程内缓存：record id → 分类结果。会话/轮视图反复扫描
// 账本，每条记录的 body 只需读一次盘。
var bodyClassCache sync.Map // map[string]turnClass

// classifyRecord 读该记录的 body 并分类（带缓存）；body 读不到按新轮兜底。
func classifyRecord(st *store.Store, r store.Record) turnClass {
	if c, ok := bodyClassCache.Load(r.ID); ok {
		return c.(turnClass)
	}
	c := turnClass{starts: true}
	if b, err := st.Body(r.ID); err == nil && len(b.RequestBody) > 0 {
		c.starts, c.text = classifyBody(b.RequestBody)
	}
	bodyClassCache.Store(r.ID, c)
	return c
}

// classifyBody 判断一次模型请求是否开启新一轮用户请求。
// 返回（是否新轮，新轮的用户输入文本）。
func classifyBody(body []byte) (bool, string) {
	if len(body) == 0 {
		return true, ""
	}
	// 顶层消息数组：messages（OpenAI chat / Anthropic）或 input（Responses API）
	var top struct {
		Messages []json.RawMessage `json:"messages"`
		Input    []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &top) != nil {
		return true, ""
	}
	list := top.Messages
	if len(list) == 0 {
		list = top.Input
	}
	if len(list) == 0 {
		return true, "" // 无消息数组（豆包 content_block 等私有形态）
	}
	var last struct {
		Role    string          `json:"role"`
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(list[len(list)-1], &last) != nil {
		return true, ""
	}
	// Responses API：input 元素带 type，function_call_output 是工具结果回传
	if last.Type == "function_call_output" || last.Type == "tool_result" {
		return false, ""
	}
	switch last.Role {
	case "tool", "function": // OpenAI chat 工具结果
		return false, ""
	case "user":
		// content 为块数组（Anthropic 形态）：含 tool_result 即延续，
		// 工具结果里混排的 text 多为 system-reminder 注入，不作新轮依据
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(last.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "tool_result" {
					return false, ""
				}
			}
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					return true, cutText(b.Text, turnTextMax)
				}
			}
			return true, ""
		}
		// content 为字符串：纯用户文本
		var s string
		if json.Unmarshal(last.Content, &s) == nil {
			return true, cutText(s, turnTextMax)
		}
		return true, ""
	default:
		// assistant/system 收尾等异常形态：保守自成一轮
		return true, ""
	}
}

const turnTextMax = 200

// cutText 用户输入清洗与截断，cutTitle 的可调长度版：优先取 <user_query>
// （WorkBuddy），剥掉 system-reminder 等包装标签后取剩余正文。
func cutText(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", "")
	if m := reUserQuery.FindStringSubmatch(s); m != nil {
		s = m[1]
	} else {
		s = reSysReminder.ReplaceAllString(s, "")
		s = reAnyTag.ReplaceAllString(s, " ")
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
		return strings.TrimSpace(string(r)) + "…"
	}
	return strings.TrimSpace(string(r))
}

// buildTurns 把一组按 (session, subagent) 归内、时间升序的记录切成用户轮次。
// 组内首条即延续（会话跨天中断）时开隐式轮承接。
func buildTurns(recs []store.Record, classify func(store.Record) turnClass) []UserTurn {
	if len(recs) == 0 {
		return nil
	}
	// 防御性按时间稳定排序（账本文件序 ≈ 时间序，缓冲写入可能有乱序）
	recs = append([]store.Record(nil), recs...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Time.Before(recs[j].Time) })

	turns := []UserTurn{}
	var cur *UserTurn
	for _, r := range recs {
		c := classify(r)
		if cur == nil || c.starts {
			turns = append(turns, UserTurn{
				ID:       r.ID,
				Session:  r.Session,
				Subagent: r.Subagent,
				Agent:    r.Agent,
				Time:     r.Time.Format(time.RFC3339),
			})
			cur = &turns[len(turns)-1]
			cur.Text = c.text
		}
		cur.Calls++
		if r.Error != "" || r.Status >= 400 {
			cur.Errors++
		}
		if r.Usage != nil {
			cur.Usage.Input += r.Usage.Input
			cur.Usage.Output += r.Usage.Output
			cur.Usage.CacheRead += r.Usage.CacheRead
			cur.Usage.CacheWrite += r.Usage.CacheWrite
			cur.Usage.Reasoning += r.Usage.Reasoning
		}
		for _, t := range r.ToolCalls {
			if t.Name != "" && !containsStr(cur.Tools, t.Name) {
				cur.Tools = append(cur.Tools, t.Name)
			}
		}
		if e := timeOf(cur.EndTime); r.Time.After(e) {
			cur.EndTime = r.Time.Format(time.RFC3339)
		}
		cur.records = append(cur.records, r)
	}
	// 轮耗时：末条结束 - 首条开始（跨天隐式轮可能出现负值，钳到 0）
	for i := range turns {
		start, end := timeOf(turns[i].Time), timeOf(turns[i].EndTime)
		if d := end.Sub(start).Milliseconds() + turns[i].records[len(turns[i].records)-1].DurationMS; d > 0 {
			turns[i].DurationMS = d
		}
	}
	return turns
}

// TurnsForDay 把某一天的录制记录切成用户轮次（跨会话，时间降序）。
// 会话跨天的轮只在本天内呈现（账本按天分文件，天边界处断开）。
// 无会话归因的记录（MITM 误录等）自成一轮，避免黏进空 key 大组。
func TurnsForDay(st *store.Store, day string) []UserTurn {
	recs, err := st.Records(day)
	if err != nil {
		return nil
	}
	// Records 返回新在前，反转成时间升序
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	groups := map[string][]store.Record{}
	for _, r := range recs {
		k := r.Session + "\x00" + r.Subagent
		if r.Session == "" {
			k = "id\x00" + r.ID
		}
		groups[k] = append(groups[k], r)
	}
	var out []UserTurn
	for _, g := range groups {
		out = append(out, buildTurns(g, func(r store.Record) turnClass { return classifyRecord(st, r) })...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// filterTurns 轮次列表过滤：agent 精确、status=err 含失败调用、kw 对
// 用户文本/会话/agent 子串匹配。
func filterTurns(turns []UserTurn, agent, status, kw string) []UserTurn {
	kw = strings.ToLower(strings.TrimSpace(kw))
	out := make([]UserTurn, 0, len(turns))
	for _, t := range turns {
		if agent != "" && t.Agent != agent {
			continue
		}
		if status == "err" && t.Errors == 0 {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(t.Text+"\x00"+t.Session+"\x00"+t.Agent), kw) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// TurnCall 轮详情里的一次模型调用：记录 + 旁路聚合的响应结果。
type TurnCall struct {
	Record store.Record `json:"record"`
	Result *ir.Result   `json:"result,omitempty"`
}

// TurnDetail 找到包含该记录的轮，返回轮与逐次调用的聚合结果。
// 天定位与 recordWithBody 同款：ID 前 8 位推账本文件，推不出扫全部天。
func TurnDetail(st *store.Store, id string) (*UserTurn, []TurnCall, bool) {
	day := ""
	if len(id) >= 8 && validDay(id[:8]) {
		s := id[:8]
		day = s[:4] + "-" + s[4:6] + "-" + s[6:8]
	}
	days := []string{day}
	if day == "" {
		days = st.Days()
	}
	for _, d := range days {
		recs, err := st.Records(d)
		if err != nil {
			continue
		}
		inDay := false
		for _, r := range recs {
			if r.ID == id {
				inDay = true
				break
			}
		}
		if !inDay {
			continue
		}
		dayTurns := TurnsForDay(st, d)
		for i := range dayTurns {
			t := &dayTurns[i]
			for _, r := range t.records {
				if r.ID != id {
					continue
				}
				calls := make([]TurnCall, 0, len(t.records))
				for _, cr := range t.records {
					tc := TurnCall{Record: cr}
					if b, err := st.Body(cr.ID); err == nil {
						tc.Result = b.Result
					}
					calls = append(calls, tc)
				}
				return t, calls, true
			}
		}
	}
	return nil, nil, false
}

// sessionTurns 把一个流量会话的全部记录切成用户轮次（跨天；子代理是
// 独立 ReAct 循环，分组切轮后按时间合并，主循环即空 subagent 组）。
// 输入须已按时间升序。
func sessionTurns(st *store.Store, recs []store.Record) []UserTurn {
	groups := map[string][]store.Record{}
	for _, r := range recs {
		groups[r.Subagent] = append(groups[r.Subagent], r)
	}
	var out []UserTurn
	for _, g := range groups {
		out = append(out, buildTurns(g, func(r store.Record) turnClass { return classifyRecord(st, r) })...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}
