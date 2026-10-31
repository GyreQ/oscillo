package decode

import (
	"encoding/json"

	"ai-trace/internal/ir"
)

// Decoder 协议解码器：把上游响应（流式 SSE 帧或非流式 JSON body）解码为 IR 事件。
// 有状态（跟踪 content block / tool call 的 index 映射），每个响应流独享一个实例。
// 所有实现都必须宽容：解不动的帧直接跳过，绝不报错中断。
type Decoder interface {
	Protocol() string
	// FeedFrame 处理一帧 SSE 事件（data 为 JSON 文本）
	FeedFrame(f Frame, emit func(ir.Event))
	// FeedBody 处理非流式响应的完整 JSON body
	FeedBody(body []byte, emit func(ir.Event))
}

// 协议标识
const (
	ProtoAnthropic       = "anthropic"        // POST /v1/messages
	ProtoOpenAIChat      = "openai-chat"      // POST /v1/chat/completions
	ProtoOpenAIResponses = "openai-responses" // POST /v1/responses
	ProtoDoubao          = "doubao"           // POST /chat/completion（豆包 samantha）
	ProtoRaw             = "raw"              // 未识别协议：只存原文不解码
)

// New 按协议创建解码器。
func New(proto string) Decoder {
	switch proto {
	case ProtoAnthropic:
		return &AnthropicDecoder{}
	case ProtoOpenAIChat:
		return &OpenAIChatDecoder{}
	case ProtoOpenAIResponses:
		return &OpenAIResponsesDecoder{}
	case ProtoDoubao:
		return &DoubaoDecoder{}
	default:
		return RawDecoder{}
	}
}

// RawDecoder 未识别协议的兜底：不产出事件，原文由网关层保存。
type RawDecoder struct{}

func (RawDecoder) Protocol() string                        { return ProtoRaw }
func (RawDecoder) FeedFrame(Frame, func(ir.Event))         {}
func (RawDecoder) FeedBody([]byte, func(ir.Event))         {}

// ---------- Anthropic Messages ----------

// AnthropicDecoder 解码 Anthropic Messages API 响应。
// 流式事件参考：https://docs.anthropic.com/en/api/messages-streaming
type AnthropicDecoder struct {
	blocks map[int]antBlock // content block index -> block 信息（拿 tool_use 的 id）
}

// antUsage Anthropic 用量字段（input 不含 cache 部分）
type antUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u antUsage) toIR() ir.Usage {
	return ir.Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
}

type antBlock struct {
	Type string `json:"type"` // text | thinking | tool_use
	ID   string `json:"id"`   // tool_use 的 toolu_xxx
	Name string `json:"name"` // 工具名
}

func (d *AnthropicDecoder) Protocol() string { return ProtoAnthropic }

func (d *AnthropicDecoder) FeedFrame(f Frame, emit func(ir.Event)) {
	var m struct {
		Type         string    `json:"type"`
		Index        int       `json:"index"`
		ContentBlock antBlock  `json:"content_block"`
		Delta        antDelta  `json:"delta"`
		Message      struct {  // message_start 里的完整 message
			Usage antUsage `json:"usage"`
		} `json:"message"`
		Usage *antUsage `json:"usage"` // message_delta 里的增量用量
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
		return // 解不动的帧当心跳跳过
	}
	switch m.Type {
	case "message_start":
		if u := m.Message.Usage.toIR(); u.Input > 0 || u.CacheRead > 0 || u.CacheWrite > 0 {
			emit(ir.Event{Kind: ir.KUsage, Usage: &u})
		}
	case "content_block_start":
		if m.ContentBlock.Type == "tool_use" {
			if d.blocks == nil {
				d.blocks = map[int]antBlock{}
			}
			d.blocks[m.Index] = m.ContentBlock
			emit(ir.Event{Kind: ir.KToolStart, ToolID: m.ContentBlock.ID, Name: m.ContentBlock.Name})
		}
	case "content_block_delta":
		switch m.Delta.Type {
		case "text_delta":
			emit(ir.Event{Kind: ir.KText, Text: m.Delta.Text})
		case "thinking_delta":
			emit(ir.Event{Kind: ir.KThink, Text: m.Delta.Thinking})
		case "input_json_delta":
			// input_json_delta 帧不带工具 id，只有 block index，靠映射还原
			id := ""
			if b, ok := d.blocks[m.Index]; ok {
				id = b.ID
			}
			emit(ir.Event{Kind: ir.KToolArgs, ToolID: id, Args: m.Delta.PartialJSON})
		}
	case "message_delta":
		if m.Delta.StopReason != "" {
			emit(ir.Event{Kind: ir.KStop, Stop: m.Delta.StopReason})
		}
		if m.Usage != nil {
			if u := m.Usage.toIR(); u.Output > 0 || u.Input > 0 {
				emit(ir.Event{Kind: ir.KUsage, Usage: &u})
			}
		}
	case "error":
		emit(ir.Event{Kind: ir.KError, Err: m.Error.Message})
	}
}

// antDelta Anthropic 的 delta 结构（流式增量）
type antDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// FeedBody 解码非流式响应：content 数组里 text/thinking/tool_use 块。
func (d *AnthropicDecoder) FeedBody(body []byte, emit func(ir.Event)) {
	var m struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string    `json:"stop_reason"`
		Usage      *antUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return
	}
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			emit(ir.Event{Kind: ir.KText, Text: c.Text})
		case "thinking":
			emit(ir.Event{Kind: ir.KThink, Text: c.Thinking})
		case "tool_use":
			emit(ir.Event{Kind: ir.KToolStart, ToolID: c.ID, Name: c.Name})
			args := c.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			emit(ir.Event{Kind: ir.KToolArgs, ToolID: c.ID, Args: string(args)})
		}
	}
	if m.StopReason != "" {
		emit(ir.Event{Kind: ir.KStop, Stop: m.StopReason})
	}
	if m.Usage != nil {
		u := m.Usage.toIR()
		emit(ir.Event{Kind: ir.KUsage, Usage: &u})
	}
}
