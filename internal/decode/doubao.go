package decode

import (
	"encoding/json"

	"ai-trace/internal/ir"
)

// DoubaoDecoder 豆包桌面/网页端（samantha 协议，POST /chat/completion）的旁路解码器。
//
// SSE 事件形态（实测 2.24.10，同一段正文会在多个事件里重复，务必按事件类型取舍）：
//   - STREAM_MSG_NOTIFY：首段增量 content.content_block[].content.text_block.text
//   - CHUNK_DELTA：正文主增量流，扁平 {"text":"…"}
//   - STREAM_CHUNK（patch_object=1）：content_block 增量；patch_object=111 为
//     tts_content（CHUNK_DELTA 的语音版重复，必须跳过否则正文翻倍）
//   - FULL_MSG_NOTIFY：用户消息回显 / 最终快照，跳过（避免与增量重复）
//   - SSE_REPLY_END：终态，msg_finish_attr.brief 为完整回复摘要（兜底：整个流
//     上一个增量都没拿到时用它，覆盖缓存/重生成等无增量场景）
//   - SSE_HEARTBEAT / SSE_ACK：簿记，忽略
//
// 协议不携带 token 用量，模型仅有数字 id（ext.llm_model_type），故不产出 usage。
type DoubaoDecoder struct {
	sawText bool
}

func (d *DoubaoDecoder) Protocol() string { return ProtoDoubao }

// samanthaBlock content_block 条目（text_block / thinking_block 两种已知负载）。
type samanthaBlock struct {
	BlockType int `json:"block_type"`
	Content   struct {
		TextBlock struct {
			Text string `json:"text"`
		} `json:"text_block"`
		ThinkingBlock *struct {
			Text string `json:"text"`
		} `json:"thinking_block"`
	} `json:"content"`
}

func (d *DoubaoDecoder) emitBlocks(blocks []samanthaBlock, emit func(ir.Event)) {
	for _, blk := range blocks {
		if t := blk.Content.TextBlock.Text; t != "" {
			d.sawText = true
			emit(ir.Event{Kind: ir.KText, Text: t})
		}
		if blk.Content.ThinkingBlock != nil && blk.Content.ThinkingBlock.Text != "" {
			emit(ir.Event{Kind: ir.KThink, Text: blk.Content.ThinkingBlock.Text})
		}
	}
}

func (d *DoubaoDecoder) FeedFrame(f Frame, emit func(ir.Event)) {
	switch f.Event {
	case "STREAM_MSG_NOTIFY": // 首段：content.content_block[...]
		var m struct {
			Content struct {
				ContentBlock []samanthaBlock `json:"content_block"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(f.Data), &m) == nil {
			d.emitBlocks(m.Content.ContentBlock, emit)
		}
	case "STREAM_CHUNK": // 补丁流；只取 content_block，tts_content（111）天然漏过
		var m struct {
			PatchOp []struct {
				PatchValue struct {
					ContentBlock []samanthaBlock `json:"content_block"`
				} `json:"patch_value"`
			} `json:"patch_op"`
		}
		if json.Unmarshal([]byte(f.Data), &m) == nil {
			for _, op := range m.PatchOp {
				d.emitBlocks(op.PatchValue.ContentBlock, emit)
			}
		}
	case "CHUNK_DELTA": // 正文主增量流：扁平 {"text":"…"}
		var m struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(f.Data), &m) == nil && m.Text != "" {
			d.sawText = true
			emit(ir.Event{Kind: ir.KText, Text: m.Text})
		}
	case "SSE_REPLY_END":
		var m struct {
			EndType        int `json:"end_type"`
			MsgFinishAttr struct {
				Brief string `json:"brief"`
			} `json:"msg_finish_attr"`
		}
		if json.Unmarshal([]byte(f.Data), &m) != nil {
			return
		}
		// 整条回复没有流式增量时，用终态摘要兜底
		if !d.sawText && m.MsgFinishAttr.Brief != "" {
			emit(ir.Event{Kind: ir.KText, Text: m.MsgFinishAttr.Brief})
		}
		emit(ir.Event{Kind: ir.KStop, Stop: "end"})
	}
}

// FeedBody 豆包聊天恒为 SSE 流式；非流式 body 不产出。
func (d *DoubaoDecoder) FeedBody(body []byte, emit func(ir.Event)) {}
