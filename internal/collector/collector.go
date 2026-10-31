package collector

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Collector 扫描各 agent 的本地会话日志目录，带 mtime 缓存：
// 文件没变就不重新解析。所有方法并发安全（dashboard 请求并发到达）。
type Collector struct {
	claudeDir string
	codexDir  string

	mu    sync.Mutex
	cache map[string]*Session // 文件路径 -> 解析结果
	stamp map[string]time.Time
}

// New 创建收集器。目录不存在时对应数据源为空（不报错）。
func New() *Collector {
	home, _ := os.UserHomeDir()
	return &Collector{
		claudeDir: filepath.Join(home, ".claude", "projects"),
		codexDir:  filepath.Join(home, ".codex", "sessions"),
		cache:     map[string]*Session{},
		stamp:     map[string]time.Time{},
	}
}

// parse 各数据源的文件解析分派。
func (c *Collector) parse(path string) (*Session, error) {
	if strings.HasPrefix(path, c.codexDir) {
		return ParseCodexFile(path)
	}
	return ParseClaudeFile(path)
}

// allFiles 扫描两个数据源的全部会话文件。
func (c *Collector) allFiles() []string {
	var out []string
	// Claude：projects/<项目>/<session>.jsonl（一层项目目录）
	if ents, err := os.ReadDir(c.claudeDir); err == nil {
		for _, proj := range ents {
			if !proj.IsDir() {
				continue
			}
			files, err := os.ReadDir(filepath.Join(c.claudeDir, proj.Name()))
			if err != nil {
				continue
			}
			for _, f := range files {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
					out = append(out, filepath.Join(c.claudeDir, proj.Name(), f.Name()))
				}
			}
		}
	}
	// Codex：sessions/YYYY/MM/DD/rollout-*.jsonl（三层日期目录），直接递归
	if ents, err := os.ReadDir(c.codexDir); err == nil {
		out = append(out, walkJSONL(c.codexDir, ents, 3)...)
	}
	return out
}

// walkJSONL 递归收集 .jsonl 文件（限制深度防意外）。
func walkJSONL(dir string, ents []os.DirEntry, depth int) []string {
	var out []string
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if depth > 0 {
				sub, err := os.ReadDir(p)
				if err == nil {
					out = append(out, walkJSONL(p, sub, depth-1)...)
				}
			}
		} else if strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, p)
		}
	}
	return out
}

// SessionMeta 会话列表的轻量元数据（Entries 不下发）。
type SessionMeta struct {
	Agent      string   `json:"agent"`
	ID         string   `json:"id"`
	Project    string   `json:"project,omitempty"`
	Title      string   `json:"title,omitempty"`
	ModTime    time.Time `json:"modTime"`
	EntryCount int      `json:"entryCount"`
	TurnCount  int      `json:"turnCount"`
	TotalUsage struct {
		Input  int64 `json:"input"`
		Output int64 `json:"output"`
	} `json:"totalUsage"`
}

// Sessions 全部会话的元数据，按修改时间新在前。
// mtime 缓存：本次扫描中未变化的文件直接用缓存。
func (c *Collector) Sessions() []SessionMeta {
	c.mu.Lock()
	defer c.mu.Unlock()
	var metas []SessionMeta
	for _, path := range c.allFiles() {
		s := c.load(path)
		if s == nil {
			continue
		}
		m := SessionMeta{
			Agent: s.Agent, ID: s.ID, Project: s.Project, Title: s.Title,
			ModTime: s.ModTime, EntryCount: len(s.Entries), TurnCount: s.TurnCount,
		}
		m.TotalUsage.Input = s.TotalUsage.Prompt()
		m.TotalUsage.Output = s.TotalUsage.Output
		metas = append(metas, m)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].ModTime.After(metas[j].ModTime) })
	return metas
}

// Session 按 agent + id 找会话详情（含全部 entries）。
func (c *Collector) Session(agent, id string) (*Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, path := range c.allFiles() {
		s := c.load(path)
		if s != nil && s.Agent == agent && s.ID == id {
			return s, true
		}
	}
	return nil, false
}

// load 带缓存加载一个文件（调用方需持锁）。
func (c *Collector) load(path string) *Session {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if s, ok := c.cache[path]; ok && c.stamp[path].Equal(fi.ModTime()) {
		return s
	}
	s, err := c.parse(path)
	if err != nil {
		return nil
	}
	c.cache[path] = s
	c.stamp[path] = fi.ModTime()
	return s
}
