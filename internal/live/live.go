// Package live 实时事件总线：把网关旁路解码产生的 IR 事件流推送给
// Dashboard（SSE），让正在录制的请求在页面上逐字出现。
//
// 设计约束：
//   - 推送路径绝不影响请求本身：Publish 全程非阻塞，慢消费者直接丢消息
//   - 无订阅者时零开销（只查一次 map）
package live

import (
	"sync"

	"ai-trace/internal/ir"
	"ai-trace/internal/store"
)

// 消息类型。
const (
	TypeReqStart = "req_start" // 请求已发出（等待上游首字节）
	TypeEvent    = "event"     // 一个 IR 流式事件（text/think/tool_args 增量…）
	TypeReqEnd   = "req_end"   // 请求结束，record 为完整落盘记录
)

// Msg 推给前端的一条实时消息。同一请求靠 ID 关联。
type Msg struct {
	Type    string        `json:"type"`
	ID      string        `json:"id"`
	Agent   string        `json:"agent,omitempty"`
	Session string        `json:"session,omitempty"`
	Subagent string       `json:"subagent,omitempty"`
	Model   string        `json:"model,omitempty"`
	Proto   string        `json:"proto,omitempty"`
	Path    string        `json:"path,omitempty"`
	Method  string        `json:"method,omitempty"` // TypeReqStart
	Ev      *ir.Event     `json:"ev,omitempty"`    // TypeEvent
	Record  *store.Record `json:"record,omitempty"` // TypeReqEnd
}

// Hub 订阅/发布中心。零值可用。
type Hub struct {
	mu   sync.Mutex
	subs map[chan Msg]struct{}
}

// NewHub 创建总线。
func NewHub() *Hub { return &Hub{subs: map[chan Msg]struct{}{}} }

// Subscribe 订阅。返回消息通道与取消函数；通道关闭即表示不再有消息。
func (h *Hub) Subscribe() (<-chan Msg, func()) {
	ch := make(chan Msg, 256)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan Msg]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

// Publish 非阻塞广播：某个订阅者的缓冲满了就给它丢消息（Dashboard 掉几帧
// 无所谓，网关请求路径绝不能等）。
func (h *Hub) Publish(m Msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- m:
		default: // 缓冲满，丢弃
		}
	}
}

// Count 当前订阅者数（诊断用）。
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
