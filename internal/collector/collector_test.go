package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 写一个临时 Claude Code 会话文件并解析。
func writeClaude(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-session.jsonl")
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func joinLines(ls []string) string {
	out := ""
	for _, l := range ls {
		out += l + "\n"
	}
	return out
}

// TestParseClaudeFile 用真实结构样本验证解析：
// 用户输入（string content）、assistant（thinking + tool_use + usage）、
// tool_result 轮（数组 content）、ai-title。
func TestParseClaudeFile(t *testing.T) {
	path := writeClaude(t,
		`{"type":"ai-title","aiTitle":"MySQL 表结构初始化"}`,
		`{"type":"mode","mode":"normal"}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-10-17T10:00:00Z","message":{"role":"user","content":"帮我看下这个表结构"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-17T10:00:05Z","isSidechain":false,"message":{"role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"thinking","thinking":"先看文件"},{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"/x.sql"}}],"usage":{"input_tokens":100,"cache_read_input_tokens":50,"output_tokens":20}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-10-17T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"CREATE TABLE users..."}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-10-17T10:00:10Z","message":{"role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"text","text":"结构没问题"}],"usage":{"input_tokens":300,"output_tokens":10}}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"2026-10-17T10:00:11Z","isSidechain":true,"message":{"role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"子代理的补充"}]}}`,
	)
	s, err := ParseClaudeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "MySQL 表结构初始化" {
		t.Errorf("标题不对: %q", s.Title)
	}
	if len(s.Entries) != 5 {
		t.Fatalf("条目数不对: %d", len(s.Entries))
	}
	// 第一条：用户输入
	if s.Entries[0].Role != "user" || s.Entries[0].Text != "帮我看下这个表结构" {
		t.Errorf("首条不对: %+v", s.Entries[0])
	}
	// 第二条：assistant 带 thinking + tool_use + usage
	a := s.Entries[1]
	if a.Model != "claude-sonnet-5-5" || a.Thinking != "先看文件" ||
		len(a.ToolUses) != 1 || a.ToolUses[0].Name != "Read" {
		t.Errorf("assistant 条目不对: %+v", a)
	}
	if string(a.ToolUses[0].Args) != `{"file_path":"/x.sql"}` {
		t.Errorf("tool_use 参数不对: %s", a.ToolUses[0].Args)
	}
	if a.Usage == nil || a.Usage.Input != 100 || a.Usage.CacheRead != 50 || a.Usage.Output != 20 {
		t.Errorf("usage 不对: %+v", a.Usage)
	}
	// 第三条：tool_result 轮
	tr := s.Entries[2]
	if len(tr.ToolResults) != 1 || tr.ToolResults[0].ToolUseID != "call_1" ||
		tr.ToolResults[0].Content != "CREATE TABLE users..." {
		t.Errorf("tool_result 不对: %+v", tr.ToolResults)
	}
	// 第四条：纯文本回复
	if s.Entries[3].Text != "结构没问题" {
		t.Errorf("回复文本不对: %+v", s.Entries[3])
	}
	// 第五条：sidechain（子代理）
	if !s.Entries[4].Sidechain {
		t.Errorf("sidechain 未识别: %+v", s.Entries[4])
	}
	// 统计
	if s.TurnCount != 1 {
		t.Errorf("轮次不对: %d（tool_result 轮不应计入）", s.TurnCount)
	}
	if s.TotalUsage.Input != 400 || s.TotalUsage.Output != 30 {
		t.Errorf("累计 usage 不对: %+v", s.TotalUsage)
	}
}

// TestParseClaudeBadLines 坏行与空 content 的容错。
func TestParseClaudeBadLines(t *testing.T) {
	path := writeClaude(t,
		`{"type":"user", 坏 JSON`,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":""}}`,
		`{"type":"unknown-type","x":1}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":"只有文本"}}`,
	)
	s, err := ParseClaudeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entries) != 1 || s.Entries[0].Text != "只有文本" {
		t.Fatalf("容错解析不对: %+v", s.Entries)
	}
	if s.Title != "" {
		t.Errorf("无用户输入时标题应为空，得到 %q", s.Title)
	}
}

// TestParseCodexFile Codex rollout 样本（按公开格式构造，待真实样本验证）。
func TestParseCodexFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-10-17-test.jsonl")
	os.WriteFile(path, []byte(joinLines([]string{
		`{"timestamp":"2026-10-17T12:00:00Z","type":"session_meta","payload":{"id":"cs_1","cwd":"/Users/x/proj"}}`,
		`{"timestamp":"2026-10-17T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"跑一下测试"}]}}`,
		`{"timestamp":"2026-10-17T12:00:05Z","type":"response_item","payload":{"type":"reasoning","summary":[{"text":"先看目录"}]}}`,
		`{"timestamp":"2026-10-17T12:00:06Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"command\":\"go test ./...\"}"}}`,
		`{"timestamp":"2026-10-17T12:00:08Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok  all passed"}}`,
	})), 0o644)
	s, err := ParseCodexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Agent != "codex" || s.Project != "/Users/x/proj" {
		t.Errorf("元信息不对: %+v", s)
	}
	if len(s.Entries) != 4 {
		t.Fatalf("条目数不对: %d", len(s.Entries))
	}
	if s.Entries[2].ToolUses[0].Name != "shell" || string(s.Entries[2].ToolUses[0].Args) != `{"command":"go test ./..."}` {
		t.Errorf("function_call 不对: %+v", s.Entries[2].ToolUses)
	}
	if s.Entries[3].ToolResults[0].Content != "ok  all passed" {
		t.Errorf("function_call_output 不对: %+v", s.Entries[3].ToolResults)
	}
}

// TestCollectorCache mtime 缓存：未变化的文件不重新解析（用解析结果指针判等）。
func TestCollectorCache(t *testing.T) {
	path := writeClaude(t,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`)
	c := &Collector{cache: map[string]*Session{}, stamp: map[string]time.Time{}}
	c.load(path)
	s1 := c.cache[path]
	c.load(path) // 未变化
	if c.cache[path] != s1 {
		t.Fatal("mtime 未变时不应重新解析")
	}
}
