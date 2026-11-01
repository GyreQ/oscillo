// Package attrib 做请求归因：这是哪个 agent 的、哪个会话的、哪个子 agent 的请求。
// 三件套（借鉴 magpie）：
//  1. UA 前缀表识别 agent
//  2. session header（各 agent 自带）识别会话，兜底用首条 user message 的 hash
//  3. x-claude-code-agent-id 识别 subagent
package attrib

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Info 归因结果。
type Info struct {
	Agent    string `json:"agent"`    // agent 标识（claude-code / codex / unknown）
	Session  string `json:"session"`  // 会话 id（header 优先，否则内容 hash）
	Subagent string `json:"subagent"` // 子 agent（Claude Code 的 subagent 机制）
}

// uaAgents User-Agent 前缀表（小写匹配）。
var uaAgents = []struct{ prefix, id string }{
	{"claude-cli", "claude-code"},
	{"claude-code", "claude-code"},
	{"claude-desktop", "claude-desktop"},
	{"codex", "codex"},
	{"gemini-cli", "gemini-cli"},
	{"opencode", "opencode"},
	{"cursor", "cursor"},
	{"zcode", "zcode"},
	{"workbuddy", "workbuddy"},
	{"codebuddy", "codebuddy"},
	{"aider", "aider"},
	{"cline", "cline"},
	{"goose", "goose"},
	{"roo-cline", "roo-cline"},
	{"kimi-cli", "kimi-cli"},
	{"qwen-code", "qwen-code"},
	{"quark", "quark"},   // 夸克客户端（UA 形如 Quark/x.y.z）
	{"doubao", "doubao"}, // 豆包 CLI
	{"pi/", "pi"},        // pi coding agent（pi.dev）——UA 前缀按 pi/x.y.z 预估，实测认不出再调
	{"curl", "curl"},     // 手动调试请求
}

// uaContains 包含匹配表：标识不在 UA 开头的客户端，前缀表 miss 后按包含兜底。
var uaContains = []struct{ part, id string }{
	{"workbuddy", "workbuddy"},
	{"codebuddy", "codebuddy"},
	{"samanthadoubao", "doubao"}, // 豆包桌面端：Chrome UA 后缀 SamanthaDoubao/x
}

// sessionHeaders 各 agent 自带的会话头，按序探测。
var sessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"X-Claude-Session-Id",
	"X-Opencode-Session",
	"Session-Id",
	"X-Session-Id",
	"X-Conversation-Id",
	"Conversation-Id",
}

// subagentHeaders 子 agent 标识头。
var subagentHeaders = []string{
	"X-Claude-Code-Agent-Id",
	"X-Claude-Code-Parent-Agent-Id",
}

// Analyze 对一个进入网关的请求做归因。proto 用于会话 hash 掺盐。
// 请求体已由网关读出（body），此处解析提取首条 user message 文本。
func Analyze(r *http.Request, body []byte, proto string) Info {
	info := Info{Agent: byUA(r.UserAgent())}
	for _, h := range sessionHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			info.Session = v
			break
		}
	}
	for _, h := range subagentHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			info.Subagent = v
			break
		}
	}
	if info.Session == "" {
		info.Session = byContent(proto, body)
	}
	return info
}

func byUA(ua string) string {
	ua = strings.ToLower(ua)
	for _, t := range uaAgents {
		if strings.HasPrefix(ua, t.prefix) {
			return t.id
		}
	}
	for _, t := range uaContains {
		if strings.Contains(ua, t.part) {
			return t.id
		}
	}
	return "unknown"
}

// byContent 无会话头时：hash(协议 + 首条 user message 文本) 生成稳定会话 id。
// 同一会话里第一条 user message 不变，所以每一轮请求 hash 出同一个会话 id
// （借鉴 magpie 的 conversationID 方案）。
func byContent(proto string, body []byte) string {
	text := firstUserText(body)
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(proto + "\x00" + text))
	return "s-" + hex.EncodeToString(sum[:6])
}

// firstUserText 提取 messages 数组里第一条 role=user 消息的文本。
// 兼容 Anthropic（content 为 string 或 [{type:"text",text:...}]）和
// OpenAI（content 为 string 或 [{type:"text"|"input_text",text:...}]）两种形状。
// 只取前 4KB 参与 hash：长上下文场景整条消息可能上 MB，hash 前缀已足够稳定。
func firstUserText(body []byte) string {
	var m struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input []struct { // responses API 的输入
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	msgs := m.Messages
	if len(msgs) == 0 {
		msgs = m.Input
	}
	for _, msg := range msgs {
		if msg.Role != "user" {
			continue
		}
		if t := contentText(msg.Content); t != "" {
			if len(t) > 4096 {
				t = t[:4096]
			}
			return t
		}
	}
	return ""
}

// contentText 从 content 字段提取文本，兼容字符串与块数组两种形状。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}
