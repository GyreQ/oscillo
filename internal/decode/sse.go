// Package decode 提供旁路（tee）解码器：网关把响应字节流原样透传给客户端，
// 同时把同一份字节喂给这里的解码器提取结构化事件。解码失败只影响录制，
// 绝不影响转发——这是本产品的第一原则。
package decode

import "strings"

// Frame 一帧完整的 SSE 事件（event 名 + data 内容，多行 data 已按规范用 \n 拼接）。
type Frame struct {
	Event string
	Data  string
}

// Scanner 增量 SSE 分帧器：边写字节边产出完整帧。
// SSE 格式：每行 "field: value"，空行表示一帧结束；注释行以 ':' 开头（心跳 ping）。
// 非并发安全：每个响应流独享一个。
type Scanner struct {
	buf     strings.Builder // 当前帧的 data 累积（多行拼接）
	event   string          // 当前帧的 event 名
	pending []byte          // 不完整行的缓冲
}

// Write 喂入一段字节，返回**本次新完成**的帧（不包含历史帧，可安全重复调用）。
func (s *Scanner) Write(p []byte) []Frame {
	s.pending = append(s.pending, p...)
	var newFrames []Frame
	for {
		// 按 \n 切行，行尾 \r 一并去掉
		idx := -1
		var line []byte
		for i, b := range s.pending {
			if b == '\n' {
				idx = i
				line = s.pending[:i]
				break
			}
		}
		if idx < 0 {
			break // 没有完整行，留在 pending
		}
		s.pending = s.pending[idx+1:]
		line = trimCR(line)
		s.consumeLine(line, &newFrames)
	}
	return newFrames
}

// Flush 流结束时冲刷：若缓冲里还有未以空行结束的帧，补一次分帧。
func (s *Scanner) Flush() []Frame {
	var out []Frame
	if len(s.pending) > 0 {
		line := trimCR(s.pending)
		s.pending = nil
		s.consumeLine(line, &out)
	}
	if s.buf.Len() > 0 || s.event != "" {
		out = append(out, Frame{Event: s.event, Data: s.buf.String()})
		s.buf.Reset()
		s.event = ""
	}
	return out
}

func (s *Scanner) consumeLine(line []byte, out *[]Frame) {
	switch {
	case len(line) == 0:
		// 空行：一帧结束。无内容时忽略（SSE 规范允许连续空行）
		if s.buf.Len() > 0 || s.event != "" {
			*out = append(*out, Frame{Event: s.event, Data: s.buf.String()})
			s.buf.Reset()
			s.event = ""
		}
	case line[0] == ':':
		// 注释行（keepalive ping），忽略
	case strings.HasPrefix(string(line), "data:"):
		d := strings.TrimPrefix(string(line), "data:")
		s.buf.WriteString(strings.TrimPrefix(d, " "))
		s.buf.WriteByte('\n')
	case strings.HasPrefix(string(line), "event:"):
		e := strings.TrimPrefix(string(line), "event:")
		s.event = strings.TrimPrefix(e, " ")
	case strings.HasPrefix(string(line), "id:") || strings.HasPrefix(string(line), "retry:"):
		// id / retry 字段：录制场景用不上，忽略
	default:
		// 不认识的字段行，忽略（宽容解析）
	}
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
