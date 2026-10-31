package live

import (
	"testing"
	"time"

	"ai-trace/internal/ir"
)

func TestHubBroadcast(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	defer cancel()

	h.Publish(Msg{Type: TypeReqStart, ID: "r1", Model: "glm-5.3"})
	h.Publish(Msg{Type: TypeEvent, ID: "r1", Ev: &ir.Event{Kind: ir.KText, Text: "你好"}})

	want := []string{TypeReqStart, TypeEvent}
	for i, w := range want {
		select {
		case m := <-ch:
			if m.Type != w || m.ID != "r1" {
				t.Fatalf("第 %d 条不对: %+v", i, m)
			}
		case <-time.After(time.Second):
			t.Fatalf("第 %d 条超时未收到", i)
		}
	}
}

func TestHubSlowConsumerDrops(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	defer cancel()
	// 灌满缓冲（256）再多发一批：Publish 必须立即返回且不 panic
	for i := 0; i < 300; i++ {
		h.Publish(Msg{Type: TypeEvent, ID: "r1"})
	}
	// 收到的应是前 256 条，无死锁
	seen := 0
loop:
	for {
		select {
		case <-ch:
			seen++
		default:
			break loop
		}
	}
	if seen != 256 {
		t.Errorf("慢消费者应收到 256 条，实际 %d", seen)
	}
}

func TestHubCancel(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()
	// 取消后再发布不 panic、通道已关
	h.Publish(Msg{Type: TypeReqStart})
	if _, ok := <-ch; ok {
		t.Error("取消后通道应已关闭")
	}
	if h.Count() != 0 {
		t.Errorf("取消后订阅数应为 0，实际 %d", h.Count())
	}
}

func TestHubNoSubs(t *testing.T) {
	h := NewHub()
	h.Publish(Msg{Type: TypeEvent}) // 无订阅者，直接返回
}
