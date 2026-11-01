package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zcodeSkillBody 构造 ZCode（Anthropic 协议）带 Skill 调用与正文回传的请求体。
// histCtl>0 时在历史里再放一次更早的 Skill 调用与正文（检验不重复计数）。
func zcodeSkillBody(histCtl int) []byte {
	skillResult := map[string]any{
		"role": "user",
		"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t-new", "content":
			`<skill_content name="control-browser">
# Skill: control-browser
用浏览器干活。
Base directory for this skill: /plugins/browser-use/0.5.1/skills/control-browser
</skill_content>`},
		},
	}
	lastAssistant := map[string]any{
		"role": "assistant",
		"content": []map[string]any{
			{"type": "text", "text": "好的，我调用浏览器技能。"},
			{"type": "tool_use", "id": "t-new", "name": "Skill", "input": map[string]any{"skill": "browser-use:control-browser"}},
		},
	}
	msgs := []map[string]any{{"role": "user", "content": "帮我打开页面"}}
	if histCtl > 0 {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "t-old", "name": "Skill", "input": map[string]any{"skill": "browser-use:web-gui-tester"}},
			}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "t-old", "content":
				`<skill_content name="web-gui-tester">旧正文</skill_content>`},
			}},
		)
	}
	msgs = append(msgs, lastAssistant, skillResult)
	b, _ := json.Marshal(map[string]any{
		"model":    "test",
		"messages": msgs,
		"tools":    []map[string]any{{"name": "Skill", "description": "Invoke a skill."}},
	})
	return b
}

func TestExtractZCodeSkill(t *testing.T) {
	caps := Extract(zcodeSkillBody(0))
	if len(caps) != 1 {
		t.Fatalf("应恰好 1 条捕获（调用+正文配对），得 %d: %+v", len(caps), caps)
	}
	c := caps[0]
	if c.Name != "browser-use:control-browser" {
		t.Errorf("名字应取调用的限定名: %q", c.Name)
	}
	if !strings.Contains(c.Content, "用浏览器干活") || strings.HasPrefix(c.Content, "<skill_content") {
		t.Errorf("正文应是 skill_content 内部（不含包装标签）: %q", c.Content[:min(60, len(c.Content))])
	}
	if c.Source != "/plugins/browser-use/0.5.1/skills/control-browser" {
		t.Errorf("源目录应取 Base directory 行: %q", c.Source)
	}
}

func TestExtractZCodeSkillHistoryNotRecounted(t *testing.T) {
	// 历史里已有一轮更早的 Skill 调用：只应计最后一条 assistant 的那次
	caps := Extract(zcodeSkillBody(1))
	if len(caps) != 1 {
		t.Fatalf("历史重放不应重复计数，得 %d: %+v", len(caps), caps)
	}
	if caps[0].Name != "browser-use:control-browser" {
		t.Errorf("应计最新一次调用: %+v", caps[0])
	}
}

func TestExtractOpenAIToolCall(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "部署"},
			{"role": "assistant", "content": "好", "tool_calls": []map[string]any{
				{"id": "c1", "type": "function", "function": map[string]any{
					"name": "Skill", "arguments": `{"skill":"deploy"}`,
				}},
			}},
		},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "Skill",
				"description": "Invoke.\nskills under folder: /skills\n- deploy: 部署技能\n- review: 评审技能",
			},
		}},
	})
	caps := Extract(b)
	if len(caps) != 1 || caps[0].Name != "deploy" {
		t.Fatalf("OpenAI 形态应得 deploy: %+v", caps)
	}
	if caps[0].Source != "/skills/deploy" {
		t.Errorf("注册表应补全源目录: %q", caps[0].Source)
	}
}

func TestExtractEchoFiltered(t *testing.T) {
	// 技能正文只认 Skill 工具的结果：其它工具（Bash/Read）结果里引用的
	// skill_content 文本（agent 恰好读了技能文件或检测器源码）是引用回声，
	// 不算沉淀。tool_use_id 把结果配回调用方工具名。
	b, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "看看技能文件"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "b1", "name": "Bash", "input": map[string]any{"command": "cat skill.md"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "b1", "content":
				`输出：<skill_content name="control-browser">被引用的正文</skill_content>`},
			}},
		},
	})
	if caps := Extract(b); len(caps) != 0 {
		t.Fatalf("非 Skill 工具结果里的技能文本不应计数: %+v", caps)
	}
}

func TestExtractExpertAndCommandUnchanged(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": "好"},
			{"role": "user", "content": "<command-name>/review</command-name><command-content>看代码</command-content>"},
		},
	})
	// command-name 记使用事件（命令展开是当轮的最后一条 user 消息）
	caps := Extract(b)
	found := false
	for _, c := range caps {
		if c.Name == "review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("command-name 应记使用事件: %+v", caps)
	}
	// 历史里的旧命令展开不重复计：命令消息后面再有一条 user 消息
	b2, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "<command-name>/review</command-name>"},
			{"role": "assistant", "content": "好"},
			{"role": "user", "content": "继续"},
		},
	})
	if caps := Extract(b2); len(caps) != 0 {
		t.Fatalf("历史命令展开不应重复计数: %+v", caps)
	}
	// 消息中后段的 <command-name> 是引用回声（如 agent 读了命令定义文件），不算使用
	b3, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": strings.Repeat("工具输出占位。", 80) + "文件内容: <command-name>/goal</command-name>\\n<command-message>"},
		},
	})
	if caps := Extract(b3); len(caps) != 0 {
		t.Fatalf("中后段的命令标签引用不应计数: %+v", caps)
	}
}

func TestCaptureWritesAndDedups(t *testing.T) {
	root := t.TempDir()
	meta := Meta{Agent: "zcode", Session: "s1", Model: "m"}
	body := zcodeSkillBody(0)
	if n := Capture(root, body, meta); n != 1 {
		t.Fatalf("首次捕获应 1 条: %d", n)
	}
	// 同一正文再来两条请求（历史重放/下一条请求）：正文不重复归档，事件照记
	Capture(root, body, meta)
	Capture(root, body, meta)

	latest, err := os.ReadFile(filepath.Join(root, "skills", "browser-use-control-browser.md"))
	if err != nil || !strings.Contains(string(latest), "用浏览器干活") {
		t.Fatalf("最新正文应落盘: %v", err)
	}
	hb, err := os.ReadFile(filepath.Join(root, "skills", "history", "browser-use-control-browser-" + latestHash(t, string(latest)) + ".md"))
	if err != nil {
		t.Fatalf("归档应存在: %v", err)
	}
	if len(hb) != len(latest) {
		t.Errorf("归档应按指纹幂等（不重复追加）: %d vs %d", len(hb), len(latest))
	}
	list := List(root)
	if len(list) != 1 || list[0].Name != "browser-use:control-browser" || list[0].Usages != 3 {
		t.Fatalf("使用事件应按请求记 3 次: %+v", list)
	}
}

func latestHash(t *testing.T, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:12]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
