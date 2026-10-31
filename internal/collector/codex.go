package collector

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"ai-trace/internal/ir"
)

// ParseCodexFile 解析 Codex CLI 的会话文件（rollout jsonl）。
//
// 文件位置：~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
// 每行形状：{"timestamp": "...", "type": "...", "payload": {...}}
// 关心的 type：
//   - session_meta: payload {id, cwd}（会话元信息）
//   - response_item: payload {type: message|function_call|function_call_output|reasoning, ...}
//
// 注意：格式按公开资料与本仓库调研推断编写，本机未装 Codex，待真实样本验证
// （见 HANDOFF-phase2）。
func ParseCodexFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{
		Agent: "codex",
		ID:    strings.TrimSuffix(filepath.Base(path), ".jsonl"),
	}
	if fi, err := f.Stat(); err == nil {
		s.ModTime = fi.ModTime()
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var line struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				ID       string `json:"id"`
				Cwd      string `json:"cwd"`
				Type     string `json:"type"` // response_item 的子类型
				Role     string `json:"role"`
				Name     string `json:"name"`
				CallID   string `json:"call_id"`
				Args     string `json:"arguments"`
				Output   string `json:"output"`
				Content  []struct {
					Type string `json:"type"` // input_text | output_text
					Text string `json:"text"`
				} `json:"content"`
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
			} `json:"payload"`
		}
		if unmarshalLoose(sc.Bytes(), &line) != nil {
			continue
		}
		ts := parseTime(line.Timestamp)
		switch line.Type {
		case "session_meta":
			s.Project = line.Payload.Cwd
			if line.Payload.ID != "" {
				s.ID = line.Payload.ID
			}
		case "response_item":
			p := line.Payload
			switch p.Type {
			case "message":
				e := Entry{Role: p.Role, Time: ts}
				for _, c := range p.Content {
					e.Text += c.Text
				}
				if e.Text != "" {
					s.Entries = append(s.Entries, e)
				}
			case "reasoning":
				e := Entry{Role: "assistant", Time: ts}
				for _, sm := range p.Summary {
					e.Thinking += sm.Text
				}
				if e.Thinking != "" {
					s.Entries = append(s.Entries, e)
				}
			case "function_call":
				e := Entry{Role: "assistant", Time: ts}
				e.ToolUses = append(e.ToolUses, ir.ToolCall{
					ID: p.CallID, Name: p.Name, Args: []byte(p.Args),
				})
				s.Entries = append(s.Entries, e)
			case "function_call_output":
				e := Entry{Role: "user", Time: ts}
				e.ToolResults = append(e.ToolResults, ToolResult{
					ToolUseID: p.CallID,
					Content:   p.Output,
				})
				s.Entries = append(s.Entries, e)
			}
		case "turn_context":
			// 后续如需 model/effort 信息可在此提取
		}
	}
	if s.Title == "" {
		for _, e := range s.Entries {
			if e.Role == "user" && e.Text != "" {
				s.Title = truncate(e.Text, 60)
				break
			}
		}
	}
	for _, e := range s.Entries {
		if e.Role == "user" && e.Text != "" {
			s.TurnCount++
		}
	}
	return s, nil
}

// ParseCodexFile 结束。model/effort 信息在 turn_context 行，需要时再提取。
