package attrib

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func req(t *testing.T, ua string, headers map[string]string, body any) *http.Request {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
		raw = nil
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(body)
	}
	r, err := http.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("User-Agent", ua)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestAgentByUA UA 前缀识别。
func TestAgentByUA(t *testing.T) {
	cases := map[string]string{
		"claude-cli/2.0.1 (external, cli)": "claude-code",
		"codex_cli_rs/0.42.0":              "codex",
		"zcode/1.0":                        "zcode",
		"WorkBuddy/2.5 Electron":           "workbuddy",
		"curl/8.7.1":                       "curl",
		"something-new/1.0":                "unknown",
	}
	for ua, want := range cases {
		if got := byUA(ua); got != want {
			t.Errorf("UA %q → %q，期望 %q", ua, got, want)
		}
	}
}

// TestSessionByHeader session 头优先。
func TestSessionByHeader(t *testing.T) {
	r := req(t, "claude-cli/1.0", map[string]string{
		"X-Claude-Code-Session-Id": "sess-abc",
	}, `{"model":"m","messages":[{"role":"user","content":"第一句话"}]}`)
	info := Analyze(r, []byte(`{"model":"m","messages":[{"role":"user","content":"第一句话"}]}`), "anthropic")
	if info.Agent != "claude-code" || info.Session != "sess-abc" {
		t.Fatalf("归因不对: %+v", info)
	}
}

// TestSessionByContentHash 无会话头时按内容 hash，且同会话多轮 hash 稳定
// （后续轮次里第一条 user message 不变，只有 assistant/user 追加在尾部）。
func TestSessionByContentHash(t *testing.T) {
	mk := func(msgs string) []byte {
		return []byte(`{"model":"m","messages":` + msgs + `}`)
	}
	round1 := mk(`[{"role":"user","content":"帮我写个demo"}]`)
	round2 := mk(`[{"role":"user","content":"帮我写个demo"},{"role":"assistant","content":"好"},{"role":"user","content":"继续"}]`)

	r1 := req(t, "codex/1.0", nil, "")
	i1 := Analyze(r1, round1, "anthropic")
	i2 := Analyze(req(t, "codex/1.0", nil, ""), round2, "anthropic")

	if i1.Session == "" || !strings.HasPrefix(i1.Session, "s-") {
		t.Fatalf("hash 会话 id 不对: %q", i1.Session)
	}
	if i1.Session != i2.Session {
		t.Fatalf("同会话不同轮 hash 不稳定: %q vs %q", i1.Session, i2.Session)
	}
}

// TestSubagentHeader 子 agent 头。
func TestSubagentHeader(t *testing.T) {
	r := req(t, "claude-cli/1.0", map[string]string{
		"X-Claude-Code-Agent-Id": "explore",
	}, `{}`)
	info := Analyze(r, []byte(`{}`), "anthropic")
	if info.Subagent != "explore" {
		t.Fatalf("subagent 不对: %+v", info)
	}
}

// TestFirstUserTextContentShapes content 字符串与块数组两种形状。
func TestFirstUserTextContentShapes(t *testing.T) {
	if got := firstUserText([]byte(`{"messages":[{"role":"user","content":"纯文本"}]}`)); got != "纯文本" {
		t.Errorf("字符串形状: %q", got)
	}
	block := `{"messages":[{"role":"user","content":[{"type":"text","text":"块A"},{"type":"text","text":"块B"}]}]}`
	if got := firstUserText([]byte(block)); got != "块A块B" {
		t.Errorf("块数组形状: %q", got)
	}
	// 跳过非 user 消息
	lead := `{"messages":[{"role":"assistant","content":"前史"},{"role":"user","content":"第一条用户"}]}`
	if got := firstUserText([]byte(lead)); got != "第一条用户" {
		t.Errorf("跳过 assistant: %q", got)
	}
}

// TestByUAWorkBuddy WorkBuddy 的 UA 是 "CLI/<ver> WorkBuddy/<ver>"，
// 标识不在开头：验证包含匹配兜底生效，且不误伤其他 "cli/" 开头的 UA。
func TestByUAWorkBuddy(t *testing.T) {
	if got := byUA("CLI/3.1.2 WorkBuddy/5.6.100"); got != "workbuddy" {
		t.Errorf("WorkBuddy UA 识别失败: %s", got)
	}
	if got := byUA("cli/9.9 SomethingElse/1.0"); got != "unknown" {
		t.Errorf("无关 UA 不应误识别: %s", got)
	}
	if got := byUA("CodeBuddy/2.0"); got != "codebuddy" {
		t.Errorf("CodeBuddy 前缀识别失败: %s", got)
	}
}

// TestAgentByUADoubaoQuark 桌面端/客户端 UA 兜底识别：豆包（Chrome 后缀）、夸克（前缀）。
func TestAgentByUADoubaoQuark(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 SamanthaDoubao/2.24.10": "doubao",
		"Quark/7.0.2 (Macintosh; Intel Mac OS X 15_0)": "quark",
	}
	for ua, want := range cases {
		if got := byUA(ua); got != want {
			t.Errorf("byUA(%q) = %q, want %q", ua, got, want)
		}
	}
}
