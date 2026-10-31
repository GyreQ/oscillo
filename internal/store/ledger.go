// Package store 账本存储：JSONL append-only 元数据 + 每请求一个完整 body 文件。
// 原则（借鉴 magpie）：记账失败绝不弄断请求——所有写入错误吞掉记日志。
//
// 目录布局：
//
//	~/.ai-trace/
//	  traces/2026-01-17.jsonl   # 一天一个账本，一行一个请求的元数据
//	  bodies/<record-id>.json   # 完整请求/响应体（含脱敏 header、原始 SSE 文本）
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ai-trace/internal/ir"
)

// Record 账本一行：一个请求的元数据与聚合摘要。
type Record struct {
	ID         string     `json:"id"`
	Time       time.Time  `json:"time"`
	Agent      string     `json:"agent"`
	Session    string     `json:"session,omitempty"`
	Subagent   string     `json:"subagent,omitempty"`
	Protocol   string     `json:"protocol"`
	Method     string     `json:"method"`
	Path       string     `json:"path"`
	Status     int        `json:"status"`
	Model      string     `json:"model,omitempty"`
	Usage      *ir.Usage  `json:"usage,omitempty"`
	StopReason string     `json:"stopReason,omitempty"`
	ToolCalls  []ir.ToolCall `json:"toolCalls,omitempty"`
	DurationMS int64      `json:"durationMs"`
	TTFTMS     int64      `json:"ttftMs,omitempty"`
	ReqBytes   int64      `json:"reqBytes"`
	RespBytes  int64      `json:"respBytes"`
	Error      string     `json:"error,omitempty"`
}

// FullBody 一个请求的完整录制内容（bodies/<id>.json）。
type FullBody struct {
	ID              string            `json:"id"`
	RequestHeaders  map[string]string `json:"requestHeaders,omitempty"`  // 已脱敏
	RequestBody     json.RawMessage   `json:"requestBody,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"` // 已脱敏
	ResponseBody    string            `json:"responseBody,omitempty"`    // SSE 原文或 JSON 原文
	Result          *ir.Result        `json:"result,omitempty"`          // 聚合结果
}

// Store 存储根目录。
type Store struct {
	root string
}

// Open 打开（必要时创建）数据目录。
func Open(root string) (*Store, error) {
	for _, d := range []string{root, filepath.Join(root, "traces"), filepath.Join(root, "bodies")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录: %w", err)
		}
	}
	return &Store{root: root}, nil
}

// Root 数据根目录。
func (s *Store) Root() string { return s.root }

// Append 落盘一条记录：账本行 + body 文件。错误只记日志不上抛（调用方在请求路径上）。
func (s *Store) Append(rec Record, body FullBody) {
	recJSON, err := json.Marshal(rec)
	if err == nil {
		err = appendFile(filepath.Join(s.root, "traces", time.Now().Format("2006-01-02")+".jsonl"),
			append(recJSON, '\n'))
	}
	if err != nil {
		logErr("写账本", err)
	}
	body.ID = rec.ID
	bodyJSON, err := json.Marshal(body)
	if err == nil {
		err = appendFile(filepath.Join(s.root, "bodies", rec.ID+".json"), bodyJSON)
	}
	if err != nil {
		logErr("写body", err)
	}
}

// Records 读某天的全部记录（新在前）。day 格式 2006-01-02；空串表示今天。
func (s *Store) Records(day string) ([]Record, error) {
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	data, err := os.ReadFile(filepath.Join(s.root, "traces", day+".jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []Record
	for _, line := range splitLines(data) {
		var r Record
		if json.Unmarshal(line, &r) == nil {
			recs = append(recs, r)
		}
		// 解不开的行跳过（可能写了一半被杀）
	}
	// 反转成新在前
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs, nil
}

// Body 读某个请求的完整录制内容。
func (s *Store) Body(id string) (*FullBody, error) {
	// id 做一层清洗，防路径穿越
	id = filepath.Base(id)
	data, err := os.ReadFile(filepath.Join(s.root, "bodies", id+".json"))
	if err != nil {
		return nil, err
	}
	var b FullBody
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// DeleteBy 删除全部天里满足条件的记录（重写账本文件 + 删 body 文件），
// 返回删除条数。清理 MITM 误录的杂流量等维护场景用；不在请求热路径上。
// 原子性：先写同目录临时文件再 rename，中途失败不损坏原账本。
func (s *Store) DeleteBy(pred func(Record) bool) (int, error) {
	deleted := 0
	for _, day := range s.Days() {
		path := filepath.Join(s.root, "traces", day+".jsonl")
		data, err := os.ReadFile(path)
		if err != nil {
			continue // 读不动的文件不动它
		}
		var kept [][]byte
		dayDeleted := 0
		for _, line := range splitLines(data) {
			var r Record
			if json.Unmarshal(line, &r) == nil && pred(r) {
				dayDeleted++
				// 对应 body 一并清掉（失败不影响账本重写）
				os.Remove(filepath.Join(s.root, "bodies", r.ID+".json"))
				continue
			}
			kept = append(kept, line)
		}
		if dayDeleted == 0 {
			continue
		}
		deleted += dayDeleted
		var out []byte
		for _, line := range kept {
			out = append(out, line...)
			out = append(out, '\n')
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, out, 0o644); err != nil {
			return deleted, fmt.Errorf("写临时账本: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return deleted, fmt.Errorf("替换账本: %w", err)
		}
	}
	return deleted, nil
}

// Days 有账本数据的日期列表（新在前）。
func (s *Store) Days() []string {
	ents, err := os.ReadDir(filepath.Join(s.root, "traces"))
	if err != nil {
		return nil
	}
	var days []string
	for _, e := range ents {
		name := e.Name()
		if len(name) == len("2006-01-02.jsonl") && name[10:] == ".jsonl" {
			days = append(days, name[:10])
		}
	}
	// ReadDir 已按文件名排序（即日期序），反转成新在前
	for i, j := 0, len(days)-1; i < j; i, j = i+1, j-1 {
		days[i], days[j] = days[j], days[i]
	}
	return days
}

// NewID 生成记录 id：时间戳 + 随机后缀。
func NewID() string {
	var b [3]byte
	if _, err := randRead(b[:]); err != nil {
		b = [3]byte{0x01, 0x02, 0x03} // 极端情况退化到固定后缀，仍有时间戳保证唯一
	}
	return fmt.Sprintf("%s-%x", time.Now().Format("20060102-150405"), b)
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				out = append(out, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

func logErr(what string, err error) {
	fmt.Fprintf(os.Stderr, "[ai-trace] %s失败（不影响请求）: %v\n", what, err)
}
