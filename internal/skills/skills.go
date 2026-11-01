// Package skills 技能沉淀：AI Agent 在请求里注入的技能/专家包正文，
// 从旁路录制的请求体中解析出来，去重后落盘到 <dataDir>/skills/，
// 供 Dashboard 浏览、统计使用频率、回溯版本演化。
//
// 识别的形态（后三类都只从「最后一条」对应角色的消息取，协议每条请求
// 都重放完整历史，全量扫描会把一次使用重复计几百次）：
//   - WorkBuddy Expert：<expert_selection> 块（最后一条 user 消息），
//     含 frontmatter（name 等）+ 完整提示词正文，另有插件本地路径可提取
//   - 斜杠命令展开：<command-name> 标签（最后一条 user 消息，仅记使用事件）
//   - Skill 工具调用（ZCode / Claude Code 等，OpenAI tool_calls 与
//     Anthropic tool_use 块双形态）：input 里的 skill/name/command，
//     只看最后一条 assistant 消息
//   - 技能正文：<skill_content name="..."> 块（Skill 工具结果回传，
//     最后一条 user 消息），与调用事件在同一条请求相遇后按名字配对
//     合并；Base directory 行给出本地技能包目录
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
	reFMStart    = regexp.MustCompile(`\r?\n---\r?\n`)                                       // frontmatter 起始分隔线
	reFolder     = regexp.MustCompile(`^skills under folder: (.+)$`)
	reRegEntry   = regexp.MustCompile(`^- ([A-Za-z0-9_.\x{4e00}-\x{9fff}-]+):`) // 注册表条目（含中文技能名，如"发布为应用"）
	reFMName     = regexp.MustCompile(`(?m)^name:\s*["']?([^\s"']+)`)
	rePluginPath = regexp.MustCompile(`Expert plugin path:\s*\n(\S+)`)
	reExpertName = regexp.MustCompile(`selected the "(.+?)"`) // 兜底：块内无 frontmatter 时
	reCmdName    = regexp.MustCompile(`<command-name>([^<\n]+)</command-name>`)

	// ZCode / Claude Code 的 Skill 工具结果回传：正文块 + 本地基目录
	reSkillContent = regexp.MustCompile(`(?s)<skill_content[^>]*\bname="([^"]*)"[^>]*>(.*?)</skill_content>`)
	reSkillTitle   = regexp.MustCompile(`(?m)^# Skill: (.+)$`) // name 属性缺失时兜底
	reSkillBaseDir = regexp.MustCompile(`(?m)^Base directory for this skill: (.+)$`)

	// 写盘互斥：网关并发请求可能同时命中同一技能
	mu sync.Mutex
)

// cmdNameHeadRoom 命令展开标签允许出现的位置上限（消息开头的字节数）。
const cmdNameHeadRoom = 300

// wireMsg 双协议消息的统一视图：OpenAI chat 的 messages、Anthropic 的
// messages、Responses API 的 input 共用；content 可能是字符串或块数组。
type wireMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// OpenAI chat 的工具调用（Anthropic 是 content 里的 tool_use 块）
	ToolCalls []struct {
		ID string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// wireBlk content 块数组里的元素（Anthropic 形态）。
type wireBlk struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`          // tool_use 的调用 id
	ToolUseID string          `json:"tool_use_id"` // tool_result 回传的调用 id
	Name      string          `json:"name"`        // tool_use 的工具名
	Text    string          `json:"text"`    // text 块
	Content json.RawMessage `json:"content"` // tool_result 的负载（字符串或嵌套块）
	Input   json.RawMessage `json:"input"`   // tool_use 的参数对象
}

// wireTool tools 定义：Anthropic 顶层 name/description，OpenAI 在 function 里。
type wireTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Function    struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"function"`
}

// Extract 从请求体解析技能记录。调用事件恰好计一次：Skill 调用只看最后
// 一条 assistant 消息（历史里的旧调用不再计），正文只看最后一条 user
// 消息（skill_content 随历史重放同理），两者在同一条请求相遇后按名字
// 配对合并。
func Extract(body []byte) []Captured {
	var req struct {
		Messages []wireMsg  `json:"messages"`
		Input    []wireMsg  `json:"input"` // Responses API
		Tools    []wireTool `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	msgs := req.Messages
	if len(msgs) == 0 {
		msgs = req.Input
	}
	if len(msgs) == 0 {
		return nil
	}

	var out []Captured
	contents := []Captured{} // 最后一条 user 消息里的 <skill_content> 正文

	// tool_use id → 工具名（全历史）：技能正文只认 Skill 工具的结果，
	// 其它工具结果里引用的技能文本（agent 恰好读了技能文件/本检测器的
	// 源码）是引用回声，不算。
	toolNames := map[string]string{}
	for _, m := range msgs {
		for _, blk := range blocksOf(m.Content) {
			if blk.Type == "tool_use" {
				toolNames[blk.ID] = blk.Name
			}
		}
		for _, tc := range m.ToolCalls {
			toolNames[tc.ID] = tc.Function.Name
		}
	}

	// ---- 注入类形态：只看最后一条 user 消息。命令展开与专家选择随
	// 历史重放（每条后续请求都带着），全量扫描会把一次使用重复计成
	// 每条请求一次；与调用事件同规则，恰好计一次。 ----
	if i := lastRole(msgs, "user"); i >= 0 {
		for _, blk := range blocksOf(msgs[i].Content) {
			if blk.Type == "tool_result" {
				// 技能正文：Skill 工具结果里的 <skill_content>
				if !strings.EqualFold(toolNames[blk.ToolUseID], "skill") {
					continue
				}
				for _, mm := range reSkillContent.FindAllStringSubmatch(blockText(blk.Content), -1) {
					contents = append(contents, skillFromContent(mm[1], mm[2]))
				}
				continue
			}
			// 用户文本：命令展开 / 专家选择
			text := blk.Text
			for _, e := range reExpertSel.FindAllStringSubmatch(text, -1) {
				if c, ok := parseExpert(e[1]); ok {
					out = append(out, c)
				}
			}
			// 命令展开只在消息开头（<command-name> 是当轮输入的起始标签）；
			// 中后段的匹配是对命令文件内容/文档的引用，不算使用。
			for _, mm := range reCmdName.FindAllStringSubmatchIndex(text, -1) {
				if mm[0] > cmdNameHeadRoom {
					continue
				}
				n := strings.TrimSpace(text[mm[2]:mm[3]])
				if n != "" {
					out = append(out, Captured{Name: strings.TrimPrefix(n, "/")})
				}
			}
		}
	}

	// ---- Skill 调用事件：最后一条 assistant 消息 ----
	invocations := []string{}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		for _, blk := range blocksOf(msgs[i].Content) {
			if blk.Type == "tool_use" && strings.EqualFold(blk.Name, "skill") {
				if n := skillArgName(blk.Input); n != "" {
					invocations = append(invocations, n)
				}
			}
		}
		for _, tc := range msgs[i].ToolCalls {
			if strings.EqualFold(tc.Function.Name, "skill") {
				if n := skillArgName(json.RawMessage(tc.Function.Arguments)); n != "" {
					invocations = append(invocations, n)
				}
			}
		}
		break // 只看最后一条
	}

	// ---- 配对：调用名（可能是 plugin:skill 限定名）接上正文与源目录 ----
	used := make([]bool, len(contents))
	for _, inv := range invocations {
		c := Captured{Name: inv}
		for i, cc := range contents {
			if used[i] {
				continue
			}
			if cc.Name == inv || cc.Name == skillShortName(inv) {
				c.Content, c.Source = cc.Content, cc.Source
				used[i] = true
				break
			}
		}
		out = append(out, c)
	}
	for i, cc := range contents {
		if !used[i] {
			out = append(out, cc) // 只有正文可见（调用不在末条等形态）
		}
	}

	// 技能注册表（Skill 工具描述）里有 name → 所在目录的映射，
	// 给没有正文的捕获（命令展开 / 纯调用事件）补全本地包目录。
	var reg map[string]string
	for _, t := range req.Tools {
		name, desc := t.Name, t.Description
		if name == "" {
			name, desc = t.Function.Name, t.Function.Description
		}
		if strings.EqualFold(name, "skill") {
			reg = registryFolders(desc)
			break
		}
	}
	if len(reg) > 0 {
		for i := range out {
			if out[i].Source == "" && out[i].Content == "" {
				if folder, ok := reg[out[i].Name]; ok {
					out[i].Source = folder + "/" + out[i].Name
				}
			}
		}
	}
	return out
}

// blocksOf content 统一为块数组视图：字符串当作单个 text 块。
func blocksOf(raw json.RawMessage) []wireBlk {
	if len(raw) == 0 {
		return nil
	}
	var blks []wireBlk
	if json.Unmarshal(raw, &blks) == nil {
		return blks
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []wireBlk{{Type: "text", Text: s}}
	}
	return nil
}

// lastRole 最后一条指定角色消息的下标；没有返回 -1。
func lastRole(msgs []wireMsg, role string) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == role {
			return i
		}
	}
	return -1
}

// skillArgName Skill 调用参数里的技能名（ZCode 用 skill，Claude Code 用
// command，兼容 name）。
func skillArgName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var args struct {
		Skill   string `json:"skill"`
		Name    string `json:"name"`
		Command string `json:"command"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	if args.Skill != "" {
		return args.Skill
	}
	if args.Command != "" {
		return args.Command
	}
	return args.Name
}

// skillShortName 限定名的末段："browser-use:control-browser" →
// "control-browser"（正文块里的 name 属性是不带插件前缀的短名）。
func skillShortName(inv string) string {
	if i := strings.LastIndexAny(inv, ":/"); i >= 0 {
		return inv[i+1:]
	}
	return inv
}

// skillFromContent 从 <skill_content> 块组装正文捕获：名字取 name 属性
// （缺了用正文标题兜底），源目录取 Base directory 行。
func skillFromContent(name, inner string) Captured {
	c := Captured{Content: strings.TrimSpace(inner)}
	if name = strings.TrimSpace(name); name != "" {
		c.Name = name
	} else if m := reSkillTitle.FindStringSubmatch(inner); m != nil {
		c.Name = strings.TrimSpace(m[1])
	}
	if m := reSkillBaseDir.FindStringSubmatch(inner); m != nil {
		c.Source = strings.TrimSpace(m[1])
	}
	return c
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

// messageText content 归一化为纯文本：字符串原样；块数组拼 text 字段，
// tool_result 块的负载（技能正文在工具结果里回传）也取出来。
func messageText(raw json.RawMessage) string {
	var sb strings.Builder
	for _, b := range blocksOf(raw) {
		sb.WriteString(b.Text)
		if b.Type == "tool_result" && len(b.Content) > 0 {
			sb.WriteString(blockText(b.Content))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// blockText tool_result 负载转纯文本：字符串原样，嵌套块数组递归。
func blockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return messageText(raw)
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

// captureContent 最新正文写 <name>.md；内容变化时旧版按指纹归档
// history/<name>-<hash>.md。归档按指纹幂等：同一请求的历史重放会反复
// 命中同一份正文，不能反复追加。
func captureContent(dir, name, hash, content string) {
	if err := os.WriteFile(filepath.Join(dir, fileSafe(name)+".md"), []byte(content), 0o644); err != nil {
		return
	}
	hp := filepath.Join(dir, "history", fileSafe(name)+"-"+hash+".md")
	if _, err := os.Stat(hp); err == nil {
		return
	}
	appendLine(hp, []byte(content))
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
