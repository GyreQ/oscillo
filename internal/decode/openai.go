package decode

import (
	"encoding/json"
	"strconv"

	"ai-trace/internal/ir"
)

// ---------- OpenAI Chat Completions ----------

// OpenAIChatDecoder 解码 /v1/chat/completions 响应（含 DeepSeek 风格的 reasoning_content、
// 工具调用增量）。thinking 与 tool_calls 的增量按 index 聚合。
type OpenAIChatDecoder struct {
	callIDs map[int]string // tool_calls 数组下标 -> call id
}

func (d *OpenAIChatDecoder) Protocol() string { return ProtoOpenAIChat }

func (d *OpenAIChatDecoder) FeedFrame(f Frame, emit func(ir.Event)) {
	if f.Data == "[DONE]" {
		return
	}
	var m struct {
		Choices []struct {
			Delta struct {
				Content         string `json:"content"`
				ReasoningContent string `json:"reasoning_content"` // DeepSeek/OpenAI 兼容思考流
				ToolCalls       []struct {
					Index    *int   `json:"index"` // 部分实现缺省，按出现顺序补
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaiUsage `json:"usage"` // 最后一个 chunk（stream_options.include_usage）
	}
	if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
		return
	}
	for _, ch := range m.Choices {
		if ch.Delta.Content != "" {
			emit(ir.Event{Kind: ir.KText, Text: ch.Delta.Content})
		}
		if ch.Delta.ReasoningContent != "" {
			emit(ir.Event{Kind: ir.KThink, Text: ch.Delta.ReasoningContent})
		}
		for i, tc := range ch.Delta.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			id, seen := d.callIDs[idx]
			if !seen {
				// 首帧：带 id 和工具名
				id = tc.ID
				if id == "" {
					id = "call_idx_" + itoa(idx)
				}
				if d.callIDs == nil {
					d.callIDs = map[int]string{}
				}
				d.callIDs[idx] = id
				emit(ir.Event{Kind: ir.KToolStart, ToolID: id, Name: tc.Function.Name})
			}
			if tc.Function.Arguments != "" {
				emit(ir.Event{Kind: ir.KToolArgs, ToolID: id, Args: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			emit(ir.Event{Kind: ir.KStop, Stop: stopToAnthropic(*ch.FinishReason)})
		}
	}
	if m.Usage != nil {
		u := m.Usage.toIR()
		emit(ir.Event{Kind: ir.KUsage, Usage: &u})
	}
}

// FeedBody 解码非流式响应。
func (d *OpenAIChatDecoder) FeedBody(body []byte, emit func(ir.Event)) {
	var m struct {
		Choices []struct {
			Message struct {
				Content         string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls       []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return
	}
	for _, ch := range m.Choices {
		if ch.Message.Content != "" {
			emit(ir.Event{Kind: ir.KText, Text: ch.Message.Content})
		}
		if ch.Message.ReasoningContent != "" {
			emit(ir.Event{Kind: ir.KThink, Text: ch.Message.ReasoningContent})
		}
		for _, tc := range ch.Message.ToolCalls {
			args := tc.Function.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			emit(ir.Event{Kind: ir.KToolStart, ToolID: tc.ID, Name: tc.Function.Name})
			emit(ir.Event{Kind: ir.KToolArgs, ToolID: tc.ID, Args: string(args)})
		}
		if ch.FinishReason != "" {
			emit(ir.Event{Kind: ir.KStop, Stop: stopToAnthropic(ch.FinishReason)})
		}
	}
	if m.Usage != nil {
		u := m.Usage.toIR()
		emit(ir.Event{Kind: ir.KUsage, Usage: &u})
	}
}

// stopToAnthropic 把 OpenAI 的 finish_reason 统一为 Anthropic 语义。
func stopToAnthropic(r string) string {
	switch r {
	case "stop":
		return "end_turn"
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return r
	}
}

// oaiUsage OpenAI 两种 API 的用量字段并集。
// 注意口径：prompt_tokens 是总输入（含缓存命中），cached_tokens 单列后 Input 要扣除。
type oaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`  // responses API
	OutputTokens     int64 `json:"output_tokens"` // responses API
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *oaiUsage) toIR() ir.Usage {
	out := ir.Usage{}
	cached := int64(0)
	switch {
	case u.PromptTokens > 0: // chat completions
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
		out.Input = u.PromptTokens - cached
		out.Output = u.CompletionTokens
	case u.InputTokens > 0: // responses
		if u.InputTokensDetails != nil {
			cached = u.InputTokensDetails.CachedTokens
		}
		out.Input = u.InputTokens - cached
		out.Output = u.OutputTokens
	}
	out.CacheRead = cached
	if u.OutputTokensDetails != nil {
		out.Reasoning = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// ---------- OpenAI Responses（Codex 使用） ----------

// OpenAIResponsesDecoder 解码 /v1/responses 的流式事件。
// function_call 的参数增量事件只带 item_id（fc_xxx），需要映射回 call_id（call_xxx）。
type OpenAIResponsesDecoder struct {
	itemToCall map[string]string // item.id -> item.call_id
}

func (d *OpenAIResponsesDecoder) Protocol() string { return ProtoOpenAIResponses }

func (d *OpenAIResponsesDecoder) FeedFrame(f Frame, emit func(ir.Event)) {
	// responses API 的 SSE 帧带 event 名，但以 data.type 为准解析
	var m struct {
		Type     string `json:"type"`
		Item     *struct {
			Type    string `json:"type"` // reasoning | message | function_call
			ID      string `json:"id"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
		} `json:"item"`
		ItemID  string `json:"item_id"`
		Delta   string `json:"delta"`
		Text    string `json:"text"`
		Response *struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Usage *oaiUsage `json:"usage"`
		} `json:"response"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
		return
	}
	switch m.Type {
	case "response.output_item.added":
		if m.Item != nil && m.Item.Type == "function_call" {
			if d.itemToCall == nil {
				d.itemToCall = map[string]string{}
			}
			callID := m.Item.CallID
			if callID == "" {
				callID = m.Item.ID
			}
			d.itemToCall[m.Item.ID] = callID
			emit(ir.Event{Kind: ir.KToolStart, ToolID: callID, Name: m.Item.Name})
		}
	case "response.output_text.delta":
		emit(ir.Event{Kind: ir.KText, Text: m.Delta})
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		emit(ir.Event{Kind: ir.KThink, Text: m.Delta})
	case "response.function_call_arguments.delta":
		id := m.ItemID
		if c, ok := d.itemToCall[m.ItemID]; ok {
			id = c
		}
		emit(ir.Event{Kind: ir.KToolArgs, ToolID: id, Args: m.Delta})
	case "response.completed":
		if m.Response != nil {
			if m.Response.Usage != nil {
				u := m.Response.Usage.toIR()
				emit(ir.Event{Kind: ir.KUsage, Usage: &u})
			}
			emit(ir.Event{Kind: ir.KStop, Stop: "end_turn"})
		}
	case "response.failed", "error":
		msg := ""
		if m.Error != nil {
			msg = m.Error.Message
		} else if m.Response != nil && m.Response.Error != nil {
			msg = m.Response.Error.Message
		}
		emit(ir.Event{Kind: ir.KError, Err: msg})
	}
}

// FeedBody 解码非流式 responses 响应：output 数组遍历。
func (d *OpenAIResponsesDecoder) FeedBody(body []byte, emit func(ir.Event)) {
	var m struct {
		Output []struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *oaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return
	}
	for _, it := range m.Output {
		switch it.Type {
		case "message":
			for _, c := range it.Content {
				emit(ir.Event{Kind: ir.KText, Text: c.Text})
			}
		case "reasoning":
			for _, s := range it.Summary {
				emit(ir.Event{Kind: ir.KThink, Text: s.Text})
			}
		case "function_call":
			callID := it.CallID
			if callID == "" {
				callID = it.ID
			}
			emit(ir.Event{Kind: ir.KToolStart, ToolID: callID, Name: it.Name})
			emit(ir.Event{Kind: ir.KToolArgs, ToolID: callID, Args: ""}) // 参数在流式事件里
		}
	}
	emit(ir.Event{Kind: ir.KStop, Stop: "end_turn"})
	if m.Usage != nil {
		u := m.Usage.toIR()
		emit(ir.Event{Kind: ir.KUsage, Usage: &u})
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
