// turns.go 用户请求轮次：把同会话的多次模型调用聚合成"一轮用户请求"。
//
// 视角翻转：账本里一行 = 一次模型调用（模型侧）；用户侧的一轮 = 一次用户
// 输入触发的 N 次调用。切轮规则看模型请求的消息列表末尾——是工具结果
// （OpenAI 的 role:"tool"、Anthropic 的 tool_result 块、Responses API 的
// function_call_output）或系统注入（ZCode mid-conversation-system 的提醒/
// 环境快照/通知）就归入当前轮继续，是真实用户文本就开启新轮；解析不了的
// 请求体（豆包 content_block 等私有形态）安全兜底为自成一轮。
package web

import (
	"encoding/json"
	"regexp"
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
		// ZCode 压缩请求（上下文满时自动生成摘要，X-Zcode-Query-Source:
		// compact）：系统触发而非用户输入，归入当前轮延续
		if c.starts && headerValue(b.RequestHeaders, "X-Zcode-Query-Source") == "compact" {
			c.starts = false
		}
	}
	bodyClassCache.Store(r.ID, c)
	return c
}

// headerValue 大小写不敏感地取一个请求头的值。
func headerValue(h map[string]string, key string) string {
	if v, ok := h[key]; ok {
		return v
	}
	lk := strings.ToLower(key)
	for k, v := range h {
		if strings.ToLower(k) == lk {
			return v
		}
	}
	return ""
}

// classifyBody 判断一次模型请求是否开启新一轮用户请求。
// 返回（是否新轮，该轮的用户输入文本——收尾消息不是真实用户发言时向前回溯）。
//
// 规则（从消息列表末尾向前看）：
//   - 收尾的 role:"system" 注入先跳过。ZCode 用 mid-conversation-system
//     beta 把 TodoWrite 提醒、环境快照、后台通知、工具回显追加在消息列表
//     末尾，这些系统消息永远不代表用户发言；
//   - 跳过后收尾是工具结果（OpenAI 的 role:"tool"/"function"、Anthropic
//     的 tool_result 块、Responses API 的 function_call_output）→ 延续；
//   - 收尾 user 消息里有真实文本 → 新轮（ZCode 会把上下文块排在真实输入
//     前面，取最后一个非注入文本块）；
//   - 收尾 user 消息没有真实文本（纯提醒/环境注入，或纯图片）→ 延续，
//     不再为注入开空轮；
//   - 跳过系统注入后收尾是 assistant → 系统通知触发的续跑 → 延续；
//     没跳过系统消息的 assistant 收尾（prefill 等罕见形态）保守自成一轮。
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
	// 跳过收尾的系统注入消息
	lastIdx, sysSkipped := -1, false
	for i := len(list) - 1; i >= 0; i-- {
		var role struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(list[i], &role) != nil || role.Role != "system" {
			lastIdx = i
			break
		}
		sysSkipped = true
	}
	if lastIdx < 0 {
		return true, "" // 整个列表只有系统消息：保守自成一轮
	}
	var last struct {
		Role    string          `json:"role"`
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(list[lastIdx], &last) != nil {
		return true, ""
	}
	// Responses API：input 元素带 type，function_call_output 是工具结果回传
	if last.Type == "function_call_output" || last.Type == "tool_result" {
		return false, turnText(list, lastIdx)
	}
	switch last.Role {
	case "tool", "function": // OpenAI chat 工具结果
		return false, turnText(list, lastIdx)
	case "user":
		if t := userText(last.Content); t != "" {
			return true, t
		}
		// 收尾 user 消息没有真实文本：纯工具结果 / 提醒环境注入 / 纯图片
		return false, turnText(list, lastIdx)
	case "assistant":
		if sysSkipped {
			// 系统通知等注入触发的续跑：用户没说话，归当前轮
			return false, turnText(list, lastIdx)
		}
		return true, "" // prefill 等异常收尾：保守自成一轮
	default:
		// 其余角色收尾等异常形态：保守自成一轮
		return true, ""
	}
}

// turnText 向前（含 upto 本身）找最近一条带真实用户发言的 user 消息，
// 作为该请求所属轮的用户输入。中途加入抓取的会话（组内首条即延续）
// 靠它把隐式轮的文本补出来。
func turnText(list []json.RawMessage, upto int) string {
	for i := upto; i >= 0; i-- {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(list[i], &m) != nil || m.Role != "user" {
			continue
		}
		if t := userText(m.Content); t != "" {
			return t
		}
	}
	return ""
}

// userText 一条 user 消息的真实用户输入文本；没有（工具结果回传、提醒
// 环境注入、纯图片）返回 ""。
func userText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	// content 为字符串：纯用户文本
	var s string
	if json.Unmarshal(content, &s) == nil {
		return cutText(s, turnTextMax)
	}
	// content 为块数组（Anthropic 形态）
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return "" // 工具结果回传不是用户发言（混排 text 多为注入标记）
		}
	}
	// 从后往前取真实文本块：ZCode 会把摘要、system-reminder 等上下文块
	// 排在真实输入前面，最后一个非注入块才是用户说的话
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type != "text" {
			continue
		}
		if c := cutText(blocks[i].Text, turnTextMax); c != "" {
			return c
		}
	}
	return ""
}

const turnTextMax = 200

// reAmbientBlock 成对标签包裹的注入段（<in-app-browser-context>…、
// <task-notification>… 等非 <system-reminder>/<user_query> 的私有包装），
// 整段剥掉而不是只剥标签token——否则包裹内的环境内容会被当成用户话。
// RE2 不支持反向引用，不做标签名配对，展示文本可接受。
var reAmbientBlock = regexp.MustCompile(`(?s)<[a-zA-Z][\w:-]*(\s[^>]*)?>.*?</[a-zA-Z][\w:-]*>`)

// cutText 用户输入清洗与截断，cutTitle 的可调长度版：优先取 <user_query>
// （WorkBuddy），剥掉 system-reminder 与成对标签包裹的注入段后取剩余正文
// （ZCode 的环境注入后面跟着「## My request for ZCode:」+ 真实请求）。
func cutText(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", "")
	if m := reUserQuery.FindStringSubmatch(s); m != nil {
		s = m[1]
	} else {
		s = reSysReminder.ReplaceAllString(s, "")
		s = reAmbientBlock.ReplaceAllString(s, " ")
		s = strings.ReplaceAll(s, "## My request for ZCode:", " ")
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
