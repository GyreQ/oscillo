// Package ir 定义统一的事件中间表示（借鉴 magpie 的 IR 设计）。
// 各协议的流式响应都解码为同一套 Event 流，再由 Collector 聚合成 Result。
// 这样 N 种协议只需要 N 个 decoder，不需要两两转换。
package ir

import "encoding/json"

// Usage 统一 token 用量口径。
// 注意：Anthropic 的 input 不含 cache 读写部分，OpenAI 的 prompt_tokens 含缓存命中，
// 统一时以各协议原始字段语义分列存放，聚合展示用 Prompt()。
type Usage struct {
	Input      int64 `json:"input"`       // 输入 token（不含缓存）
	Output     int64 `json:"output"`      // 输出 token
	CacheRead  int64 `json:"cache_read"`  // 缓存读取 token
	CacheWrite int64 `json:"cache_write"` // 缓存写入 token
	Reasoning  int64 `json:"reasoning"`   // 推理 token（部分协议单列）
}

// Merge 只在值 > 0 时覆盖：流式过程中 usage 事件可能报告多次（有的开头+结尾各一次），
// 取最后一次的非零值。这是 magpie 踩过的坑。
func (u *Usage) Merge(o Usage) {
	if o.Input > 0 {
		u.Input = o.Input
	}
	if o.Output > 0 {
		u.Output = o.Output
	}
	if o.CacheRead > 0 {
		u.CacheRead = o.CacheRead
	}
	if o.CacheWrite > 0 {
		u.CacheWrite = o.CacheWrite
	}
	if o.Reasoning > 0 {
		u.Reasoning = o.Reasoning
	}
}

// Add 累加（会话级统计用：各轮用量相加）。
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Reasoning += o.Reasoning
}

// Prompt 统一"输入规模"口径：input + cache 读 + cache 写。
func (u Usage) Prompt() int64 { return u.Input + u.CacheRead + u.CacheWrite }

// Total 总 token。
func (u Usage) Total() int64 { return u.Prompt() + u.Output }

// EventKind 流式事件类型。粒度设计参考 magpie：KToolStart 与 KToolArgs 分离
// （对应 Anthropic 的 content_block_start 与 input_json_delta），thinking 单列。
type EventKind string

const (
	KStart     EventKind = "start"      // 响应开始
	KText      EventKind = "text"       // 文本增量
	KThink     EventKind = "think"      // 思考增量
	KToolStart EventKind = "tool_start" // 工具调用开始（拿到 id 和名称）
	KToolArgs  EventKind = "tool_args"  // 工具参数 JSON 增量片段
	KStop      EventKind = "stop"       // 结束（带 stop reason）
	KUsage     EventKind = "usage"      // 用量报告
	KError     EventKind = "error"      // 错误
)

// Event 一个流式事件。
type Event struct {
	Kind  EventKind     `json:"kind"`
	Text  string        `json:"text,omitempty"`  // KText / KThink 的增量文本
	ToolID string       `json:"toolId,omitempty"` // KToolStart / KToolArgs
	Name  string        `json:"name,omitempty"`  // KToolStart 的工具名
	Args  string        `json:"args,omitempty"`  // KToolArgs 的增量 JSON 片段
	Stop  string        `json:"stop,omitempty"`  // KStop 的原因
	Usage *Usage        `json:"usage,omitempty"` // KUsage
	Err   string        `json:"err,omitempty"`   // KError 的错误信息
}

// ToolCall 聚合后的完整工具调用。
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"` // 完整参数 JSON（可能为空对象）
}

// Result 一次响应的聚合结果。
type Result struct {
	Text       string     `json:"text,omitempty"`       // 全部文本拼接
	Thinking   string     `json:"thinking,omitempty"`   // 全部思考拼接
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`  // 工具调用列表
	StopReason string     `json:"stopReason,omitempty"` // 结束原因（统一为 anthropic 语义）
	Usage      *Usage     `json:"usage,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Collector 消费 Event 流，聚合出 Result。
// 非并发安全：网关每个请求单独一个 Collector，在旁路解码路径上串行调用。
type Collector struct {
	res   Result
	tools map[string]*ToolCall // 按 id 索引正在聚合的工具调用
	order []string             // 保持出现顺序
}

// NewCollector 创建聚合器。
func NewCollector() *Collector { return &Collector{tools: map[string]*ToolCall{}} }

// OnEvent 处理一个事件。
func (c *Collector) OnEvent(e Event) {
	switch e.Kind {
	case KText:
		c.res.Text += e.Text
	case KThink:
		c.res.Thinking += e.Text
	case KToolStart:
		// Args 从空开始：KToolArgs 增量拼接，Seal 时空参数补 "{}"
		tc := &ToolCall{ID: e.ToolID, Name: e.Name}
		c.tools[e.ToolID] = tc
		c.order = append(c.order, e.ToolID)
	case KToolArgs:
		if tc, ok := c.tools[e.ToolID]; ok {
			tc.Args = append(tc.Args, e.Args...) // 拼接增量 JSON 片段
		}
	case KStop:
		c.res.StopReason = e.Stop
	case KUsage:
		if e.Usage != nil {
			if c.res.Usage == nil {
				c.res.Usage = &Usage{}
			}
			c.res.Usage.Merge(*e.Usage)
		}
	case KError:
		if c.res.Error == "" {
			c.res.Error = e.Err
		}
	}
}

// Seal 结束聚合：空参数的工具调用补 "{}"，按出现顺序生成列表。
func (c *Collector) Seal() Result {
	for _, id := range c.order {
		tc := c.tools[id]
		if len(tc.Args) == 0 {
			tc.Args = json.RawMessage("{}")
		}
		c.res.ToolCalls = append(c.res.ToolCalls, *tc)
	}
	// 响应里有工具调用但 stop 是普通结束时，改写为 tool_calls 语义，
	// 方便上层判断"这轮模型想调工具"（借鉴 magpie collector.finish）
	if len(c.res.ToolCalls) > 0 && (c.res.StopReason == "" || c.res.StopReason == "end_turn") {
		c.res.StopReason = "tool_use"
	}
	return c.res
}
