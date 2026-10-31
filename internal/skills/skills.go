// Package skills 技能沉淀：AI Agent 在请求里注入的技能/专家包正文，
// 从旁路录制的请求体中解析出来，去重后落盘到 <dataDir>/skills/，
// 供 Dashboard 浏览、统计使用频率、回溯版本演化。
//
// 目前识别三种形态（都来自请求 messages 的文本）：
//   - WorkBuddy Expert：<expert_selection> 块，含 frontmatter（name 等）
//     + 完整提示词正文，另有插件本地路径可提取
//   - 斜杠命令展开：<command-name> 标签（ZCode 等，仅记使用事件）
//   - Skill 工具调用：tool_calls 里 function.name 为 Skill/skill（仅记使用事件）
//
// 与网关的第一原则一致：这里是纯旁路，任何失败只放弃，不影响请求。
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Meta 使用事件的归因信息（来自网关的旁路分析）。
type Meta struct {
	Agent   string
	Session string
	Model   string
	At      time.Time // 事件时间；零值 = 当前时间（回填历史流量时传记录时间）
}

// Captured 从单个请求里解析出的一条技能记录。
// Content 为空表示只有使用痕迹（如 Skill 工具调用），不落正文。
type Captured struct {
	Name    string
	Content string
	Source  string // 来源路径（如 WorkBuddy 插件目录），可为空
}

// Usage index.jsonl 里的一行使用事件。
type Usage struct {
	Time    string `json:"time"`
	Name    string `json:"name"`
	Hash    string `json:"hash,omitempty"`  // 正文指纹（无正文时为空）
	Source  string `json:"source,omitempty"` // 本地技能包目录（插件路径/注册表解析）
	Model   string `json:"model,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
}

// Info List 聚合出的技能概览。
type Info struct {
	Name      string   `json:"name"`
	Hash      string   `json:"hash"`
	Source    string   `json:"source,omitempty"` // 本地技能包目录
	Size      int      `json:"size"`     // 最新正文字节数
	Usages    int      `json:"usages"`   // 使用次数
	Versions  int      `json:"versions"` // 历史版本数（去重后）
	FirstSeen string   `json:"firstSeen"`
	LastSeen  string   `json:"lastSeen"`
	Models    []string `json:"models"`
	Agents    []string `json:"agents"`
}

var (
	reExpertSel  = regexp.MustCompile(`(?s)<expert_selection[^>]*>(.*?)</expert_selection>`) // 开标签可带属性（state="active" 等）
	reFMStart    = regexp.MustCompile(`\r?\n---\r?\n`)                                      // frontmatter 起始分隔线
	reFolder     = regexp.MustCompile(`^skills under folder: (.+)$`)
	reRegEntry   = regexp.MustCompile(`^- ([A-Za-z0-9_.\x{4e00}-\x{9fff}-]+):`) // 注册表条目（含中文技能名，如"发布为应用"）
	reFMName     = regexp.MustCompile(`(?m)^name:\s*["']?([^\s"']+)`)
	rePluginPath = regexp.MustCompile(`Expert plugin path:\s*\n(\S+)`)
	reExpertName = regexp.MustCompile(`selected the "(.+?)"`) // 兜底：块内无 frontmatter 时
	reCmdName    = regexp.MustCompile(`<command-name>([^<\n]+)</command-name>`)

	// 写盘互斥：网关并发请求可能同时命中同一技能
	mu sync.Mutex
)

// Extract 从请求体（OpenAI/Anthropic 风格 messages）解析技能记录。
func Extract(body []byte) []Captured {
	var req struct {
		Messages []struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	var out []Captured
	for _, m := range req.Messages {
		text := messageText(m.Content)
		for _, blk := range reExpertSel.FindAllStringSubmatch(text, -1) {
			if c, ok := parseExpert(blk[1]); ok {
				out = append(out, c)
			}
		}
		for _, mm := range reCmdName.FindAllStringSubmatch(text, -1) {
			if n := strings.TrimSpace(mm[1]); n != "" {
				out = append(out, Captured{Name: strings.TrimPrefix(n, "/")})
			}
		}
		for _, tc := range m.ToolCalls {
			if strings.ToLower(tc.Function.Name) != "skill" {
				continue
			}
			var args struct {
				Skill string `json:"skill"`
				Name  string `json:"name"`
			}
			if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil {
				if n := args.Skill; n != "" {
					out = append(out, Captured{Name: n})
				} else if n := args.Name; n != "" {
					out = append(out, Captured{Name: n})
				}
			}
		}
	}
	// 技能注册表（Skill 工具描述）里有 name → 所在目录的映射，
	// 给没有插件路径的捕获（命令展开 / 工具调用）补全本地包目录。
	var reg map[string]string
	for _, t := range req.Tools {
		if strings.EqualFold(t.Function.Name, "skill") {
			reg = registryFolders(t.Function.Description)
			break
		}
	}
	if len(reg) > 0 {
		for i := range out {
			if out[i].Source == "" {
				if folder, ok := reg[out[i].Name]; ok {
					out[i].Source = folder + "/" + out[i].Name
				}
			}
		}
	}
	return out
}

// registryFolders 解析 Skill 工具描述里的技能注册表：
// "skills under folder: /path" 行之后的 "- name: 描述" 条目都属于该目录。
func registryFolders(desc string) map[string]string {
	out := map[string]string{}
	folder := ""
	for _, ln := range strings.Split(desc, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if m := reFolder.FindStringSubmatch(ln); m != nil {
			folder = strings.TrimSpace(m[1])
			continue
		}
		if folder == "" {
			continue
		}
		if m := reRegEntry.FindStringSubmatch(ln); m != nil {
			out[strings.TrimSpace(m[1])] = folder
		}
	}
	return out
}

// messageText content 归一化为纯文本：字符串原样；块数组拼 text 字段。
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			sb.WriteString(b.Text)
			sb.WriteByte('\n')
		}
		return sb.String()
	}
	return ""
}

// parseExpert 解析 <expert_selection> 内部：名字取 frontmatter 的 name，
// 正文从 frontmatter 起始的 --- 行开始（前面的包装说明不属于技能本身）。
func parseExpert(inner string) (Captured, bool) {
	name := ""
	if m := reFMName.FindStringSubmatch(inner); m != nil {
		name = m[1]
	} else if m := reExpertName.FindStringSubmatch(inner); m != nil {
		name = strings.TrimSpace(m[1])
	}
	if name == "" {
		return Captured{}, false
	}
	c := Captured{Name: name, Content: strings.TrimSpace(inner)}
	if m := rePluginPath.FindStringSubmatch(inner); m != nil {
		c.Source = m[1]
	}
	// 正文从 frontmatter 的第一行 --- 开始（若有；WorkBuddy 注入的是 CRLF）
	if m := reFMStart.FindStringIndex(inner); m != nil {
		c.Content = strings.TrimSpace(inner[m[0]:])
	}
	return c, true
}

// Capture 把请求里出现的技能沉淀到 <root>/skills/：
// 正文按指纹去重（变更时归档历史版本），使用事件追加 index.jsonl。
// 返回本次捕获到的技能条数（0 = 该请求不含技能）。
func Capture(root string, body []byte, meta Meta) int {
	caps := Extract(body)
	if len(caps) == 0 {
		return 0
	}
	mu.Lock()
	defer mu.Unlock()
	dir := filepath.Join(root, "skills")
	if err := os.MkdirAll(filepath.Join(dir, "history"), 0o755); err != nil {
		return 0
	}
	for _, c := range caps {
		at := meta.At
		if at.IsZero() {
			at = time.Now()
		}
		u := Usage{
			Time:    at.Format(time.RFC3339),
			Name:    c.Name,
			Source:  c.Source,
			Model:   meta.Model,
			Agent:   meta.Agent,
			Session: meta.Session,
		}
		if c.Content != "" {
			sum := sha256.Sum256([]byte(c.Content))
			u.Hash = hex.EncodeToString(sum[:])[:12]
			captureContent(dir, c.Name, u.Hash, c.Content)
		}
		if b, err := json.Marshal(u); err == nil {
			appendLine(filepath.Join(dir, "index.jsonl"), append(b, '\n'))
		}
	}
	return len(caps)
}

// captureContent 最新正文写 <name>.md；内容变化时旧版归档 history/<name>-<hash>.md。
func captureContent(dir, name, hash, content string) {
	if err := os.WriteFile(filepath.Join(dir, fileSafe(name)+".md"), []byte(content), 0o644); err != nil {
		return
	}
	appendLine(filepath.Join(dir, "history", fileSafe(name)+"-"+hash+".md"), []byte(content))
}

// List 汇总 index.jsonl 为每个技能的使用统计（按最后使用时间倒序）。
func List(root string) []Info {
	mu.Lock()
	defer mu.Unlock()
	agg := map[string]*Info{}
	hashes := map[string]map[string]bool{}
	data, err := os.ReadFile(filepath.Join(root, "skills", "index.jsonl"))
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var u Usage
		if json.Unmarshal([]byte(line), &u) != nil || u.Name == "" {
			continue
		}
		it := agg[u.Name]
		if it == nil {
			it = &Info{Name: u.Name, FirstSeen: u.Time, Models: []string{}, Agents: []string{}}
			agg[u.Name] = it
			hashes[u.Name] = map[string]bool{}
		}
		it.Usages++
		if t, ok := parseTime(u.Time); ok {
			if ft, ok2 := parseTime(it.FirstSeen); !ok2 || t.Before(ft) {
				it.FirstSeen = u.Time
			}
			if lt, ok2 := parseTime(it.LastSeen); !ok2 || t.After(lt) {
				it.LastSeen = u.Time
			}
		} else {
			it.LastSeen = u.Time
		}
		if u.Hash != "" {
			it.Hash = u.Hash
			hashes[u.Name][u.Hash] = true
		}
		if u.Source != "" {
			it.Source = u.Source
		}
		if u.Model != "" && !contains(it.Models, u.Model) {
			it.Models = append(it.Models, u.Model)
		}
		if u.Agent != "" && !contains(it.Agents, u.Agent) {
			it.Agents = append(it.Agents, u.Agent)
		}
	}
	out := make([]Info, 0, len(agg))
	for name, it := range agg {
		it.Versions = len(hashes[name])
		if b, err := os.ReadFile(filepath.Join(root, "skills", fileSafe(name)+".md")); err == nil {
			it.Size = len(b)
		}
		out = append(out, *it)
	}
	// 最后使用倒序
	sortByLastSeen(out)
	return out
}

// Detail 取某技能的最新正文与历史版本指纹列表。
func Detail(root, name string) (content string, history []string, ok bool) {
	mu.Lock()
	defer mu.Unlock()
	b, err := os.ReadFile(filepath.Join(root, "skills", fileSafe(name)+".md"))
	if err != nil {
		return "", nil, false
	}
	entries, _ := filepath.Glob(filepath.Join(root, "skills", "history", fileSafe(name)+"-*.md"))
	for _, e := range entries {
		base := strings.TrimSuffix(filepath.Base(e), ".md")
		if i := strings.LastIndex(base, "-"); i >= 0 {
			history = append(history, base[i+1:])
		}
	}
	return string(b), history, true
}

// fileSafe 文件名安全化：只保留字母数字和 .-_，其余替换为 -。
func fileSafe(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	s := strings.Trim(sb.String(), "-")
	if s == "" {
		return "unnamed"
	}
	return s
}

func appendLine(path string, b []byte) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(b)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

func sortByLastSeen(in []Info) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].LastSeen > in[j-1].LastSeen; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
