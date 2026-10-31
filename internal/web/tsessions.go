// tsessions.go 流量会话：从录制账本按 session 字段合成会话视图。
//
// 与 /api/sessions（Claude Code / Codex 本地日志解析）互补：任何被录制的
// agent（WorkBuddy / 豆包 / …）只要归因层提取出了 session id，就能在这里
// 聚合成会话——请求列表按时间排序，每条可跳转回放视图。
package web

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"ai-trace/internal/ir"
	"ai-trace/internal/store"
)

// TrafficSession 流量会话概览。
type TrafficSession struct {
	ID       string   `json:"id"` // session uuid
	Agents   []string `json:"agents"`
	Models   []string `json:"models"`
	Turns    int      `json:"turns"`    // 用户请求轮数（一轮 = 一次用户输入）
	Requests int      `json:"requests"` // 模型调用总数
	Title    string   `json:"title,omitempty"`
	First    string   `json:"firstSeen"`
	Last     string   `json:"lastSeen"`
	Usage    ir.Usage `json:"usage"`

	recs  []store.Record // 内部：记录（按时间序）
	turns []UserTurn     // 内部：切好的用户轮次（时间降序）
}

// TrafficSessions 扫描全部账本天，按 session 分组。
func TrafficSessions(st *store.Store) []*TrafficSession {
	agg := map[string]*TrafficSession{}
	for _, day := range st.Days() {
		recs, err := st.Records(day)
		if err != nil {
			continue
		}
		for _, r := range recs {
			if r.Session == "" {
				continue
			}
			ts := agg[r.Session]
			if ts == nil {
				ts = &TrafficSession{ID: r.Session}
				agg[r.Session] = ts
			}
			ts.recs = append(ts.recs, r)
			if r.Agent != "" && !containsStr(ts.Agents, r.Agent) {
				ts.Agents = append(ts.Agents, r.Agent)
			}
			if r.Model != "" && !containsStr(ts.Models, r.Model) {
				ts.Models = append(ts.Models, r.Model)
			}
			if r.Time.Before(timeOf(ts.First)) || ts.First == "" {
				ts.First = r.Time.Format(time.RFC3339)
			}
			if r.Time.After(timeOf(ts.Last)) {
				ts.Last = r.Time.Format(time.RFC3339)
			}
			if r.Usage != nil {
				ts.Usage.Input += r.Usage.Input
				ts.Usage.Output += r.Usage.Output
				ts.Usage.CacheRead += r.Usage.CacheRead
				ts.Usage.CacheWrite += r.Usage.CacheWrite
				ts.Usage.Reasoning += r.Usage.Reasoning
			}
		}
	}
	out := make([]*TrafficSession, 0, len(agg))
	for _, ts := range agg {
		sort.SliceStable(ts.recs, func(i, j int) bool { return ts.recs[i].Time.Before(ts.recs[j].Time) })
		ts.Title = sessionTitle(st, ts.recs)
		ts.turns = sessionTurns(st, ts.recs)
		ts.Turns = len(ts.turns)
		ts.Requests = len(ts.recs)
		out = append(out, ts)
	}
	// 最近活动在前
	sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	return out
}

// TrafficSessionDetail 单个流量会话（recs 已按时间序）。
func TrafficSessionDetail(st *store.Store, session string) (*TrafficSession, bool) {
	for _, ts := range TrafficSessions(st) {
		if ts.ID == session {
			return ts, true
		}
	}
	return nil, false
}

// sessionTitle 取会话首条聊天请求里的第一句用户输入做标题。
func sessionTitle(st *store.Store, recs []store.Record) string {
	for _, r := range recs {
		if !strings.Contains(r.Path, "chat/completion") && !strings.Contains(r.Path, "/messages") {
			continue
		}
		b, err := st.Body(r.ID)
		if err != nil || len(b.RequestBody) == 0 {
			continue
		}
		if t := firstUserText(b.RequestBody); t != "" {
			return t
		}
	}
	return ""
}

// firstUserText 从请求体 messages 提取第一句用户文本：
// 兼容 OpenAI（content 字符串/块数组）与豆包（content_block）两种形态。
func firstUserText(body []byte) string {
	var v struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	// 优先 role=user 的消息；没有 role 的（豆包）取第一条
	pick := -1
	for i, m := range v.Messages {
		if m.Role == "user" {
			pick = i
			break
		}
	}
	if pick < 0 && len(v.Messages) > 0 {
		pick = 0
	}
	if pick < 0 {
		return ""
	}
	m := v.Messages[pick]
	// content 为字符串
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return cutTitle(s)
	}
	// content 为块数组 [{type,text}]
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Text != "" {
				return cutTitle(b.Text)
			}
		}
	}
	// 豆包：content_block[].content.text_block.text（挂在消息级）
	var db struct {
		ContentBlock []struct {
			Content struct {
				TextBlock struct {
					Text string `json:"text"`
				} `json:"text_block"`
			} `json:"content"`
		} `json:"content_block"`
	}
	if json.Unmarshal(body, &db) == nil {
		for _, cb := range db.ContentBlock {
			if cb.Content.TextBlock.Text != "" {
				return cutTitle(cb.Content.TextBlock.Text)
			}
		}
	}
	return ""
}

// cutTitle 标题清洗与截断（60 字），正文规则见 turns.go 的 cutText。
func cutTitle(s string) string {
	return cutText(s, 60)
}

var (
	reUserQuery   = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)
	reSysReminder = regexp.MustCompile(`(?s)<system-reminder[^>]*>.*?</system-reminder>`)
	reAnyTag      = regexp.MustCompile(`<[^>\n]*>`)
)

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func timeOf(s string) time.Time {
	var t time.Time
	if s != "" {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}
