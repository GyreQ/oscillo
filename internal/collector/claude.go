// Package collector 入口 C：直接解析各 agent 的本地会话日志文件。
// 与网关录制互补——网关只录"开启之后"的流量，日志里有全部历史与会话结构
// （subagent 树、tool_result、权限决策）。
package collector

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-trace/internal/ir"
)

// ToolResult 工具执行结果（出现在下一轮 user 消息的 content 里）。
type ToolResult struct {
	ToolUseID string `json:"toolUseId"`
	Content   string `json:"content"`
	IsError   bool   `json:"isError,omitempty"`
}

// Entry 回放时间线上的一条消息。
type Entry struct {
	Time        time.Time     `json:"time"`
	Role        string        `json:"role"` // user | assistant
	Sidechain   bool          `json:"sidechain,omitempty"`
	Text        string        `json:"text,omitempty"`
	Thinking    string        `json:"thinking,omitempty"`
	ToolUses    []ir.ToolCall `json:"toolUses,omitempty"`
	ToolResults []ToolResult  `json:"toolResults,omitempty"`
	Model       string        `json:"model,omitempty"`
	Usage       *ir.Usage     `json:"usage,omitempty"`
}

// Session 一个会话文件的解析结果。
type Session struct {
	Agent      string     `json:"agent"` // claude-code | codex
	ID         string     `json:"id"`
	Project    string     `json:"project,omitempty"`
	Title      string     `json:"title,omitempty"`
	ModTime    time.Time  `json:"modTime"`
	Entries    []Entry    `json:"entries"`
	TurnCount  int        `json:"turnCount"`
	TotalUsage ir.Usage   `json:"totalUsage"`
}

// Claude Code 日志行的宽松结构（只取关心的字段）。
type claudeLine struct {
	Type        string `json:"type"`
	ParentUUID  string `json:"parentUuid"`
	UUID        string `json:"uuid"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	AiTitle     string `json:"aiTitle"`
	Message     *struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *claudeUsage    `json:"usage"`
	} `json:"message"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ParseClaudeFile 解析一个 Claude Code 会话 jsonl 文件。
// 文件位置：~/.claude/projects/<转义项目路径>/<session-id>.jsonl
// 宽容解析：坏行跳过，未知 type 跳过。
func ParseClaudeFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{
		Agent:   "claude-code",
		ID:      strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Project: projectFromDir(filepath.Base(filepath.Dir(path))),
	}
	if fi, err := f.Stat(); err == nil {
		s.ModTime = fi.ModTime()
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // 单行可能很大（tool result 内嵌文件内容）
	for sc.Scan() {
		var line claudeLine
		if unmarshalLoose(sc.Bytes(), &line) != nil {
			continue
		}
		switch line.Type {
		case "ai-title":
			s.Title = line.AiTitle
		case "user":
			e := Entry{Role: "user", Sidechain: line.IsSidechain, Time: parseTime(line.Timestamp)}
			parseUserContent(line.Message, &e)
			if e.Text != "" || len(e.ToolResults) > 0 {
				s.Entries = append(s.Entries, e)
			}
		case "assistant":
			if line.Message == nil {
				continue
			}
			e := Entry{Role: "assistant", Sidechain: line.IsSidechain, Time: parseTime(line.Timestamp), Model: line.Message.Model}
			parseAssistantContent(line.Message, &e)
			if line.Message.Usage != nil {
				u := ir.Usage{
					Input:      line.Message.Usage.InputTokens,
					Output:     line.Message.Usage.OutputTokens,
					CacheRead:  line.Message.Usage.CacheReadInputTokens,
					CacheWrite: line.Message.Usage.CacheCreationInputTokens,
				}
				e.Usage = &u
				s.TotalUsage.Add(u) // 会话累计 = 各轮相加
			}
			if e.Text != "" || e.Thinking != "" || len(e.ToolUses) > 0 {
				s.Entries = append(s.Entries, e)
			}
		}
	}
	// 标题兜底：第一条用户输入截断
	if s.Title == "" {
		for _, e := range s.Entries {
			if e.Role == "user" && e.Text != "" {
				s.Title = truncate(e.Text, 60)
				break
			}
		}
	}
	// 轮次 = 用户实际输入条数（tool_result 轮不算）
	for _, e := range s.Entries {
		if e.Role == "user" && e.Text != "" {
			s.TurnCount++
		}
	}
	return s, nil
}

// parseUserContent user 消息：content 为字符串（用户输入）或块数组（tool_result 轮）。
func parseUserContent(m *claudeLineMessage, e *Entry) {
	if m == nil {
		return
	}
	var s string
	if unmarshalLoose(m.Content, &s) == nil {
		e.Text = s
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if unmarshalLoose(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			e.Text += b.Text
		case "tool_result":
			e.ToolResults = append(e.ToolResults, ToolResult{
				ToolUseID: b.ToolUseID,
				Content:   contentToText(b.Content),
				IsError:   b.IsError,
			})
		}
	}
}

// parseAssistantContent assistant 消息：text / thinking / tool_use 块，
// 兼容纯字符串 content（部分版本/场景）。
func parseAssistantContent(m *claudeLineMessage, e *Entry) {
	if m == nil {
		return
	}
	var s string
	if unmarshalLoose(m.Content, &s) == nil {
		e.Text = s
		return
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	}
	if unmarshalLoose(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			e.Text += b.Text
		case "thinking":
			e.Thinking += b.Thinking
		case "tool_use":
			e.ToolUses = append(e.ToolUses, ir.ToolCall{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
}

// claudeLineMessage 与 claudeLine 内嵌 message 同构（解析函数共用）。
type claudeLineMessage = struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *claudeUsage    `json:"usage"`
}

// contentToText tool_result 的 content：字符串或 [{type,text}] 块。
func contentToText(raw []byte) string {
	var s string
	if unmarshalLoose(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if unmarshalLoose(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	return ""
}

// projectFromDir 目录名还原项目路径：-Users-asher-Code-ai-trace → /Users/asher/Code/ai-trace
// （不可逆转换：项目名里的 - 也会变 /，展示用足够）。
func projectFromDir(dir string) string {
	if dir == "" {
		return ""
	}
	return strings.ReplaceAll(dir, "-", "/")
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
