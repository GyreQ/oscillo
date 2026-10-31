package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-trace/internal/decode"
	"ai-trace/internal/ir"
	"ai-trace/internal/live"
	"ai-trace/internal/store"
)

// fakeUpstreamSSE 假上游固定回放的 SSE（含文本 + tool_use + usage）。
const fakeUpstreamSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":77,"cache_read_input_tokens":33}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我先看看目录"}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_77","name":"Bash"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls -la\"}"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":21}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// fakeAnthropicUpstream 假的 Anthropic 上游：记录收到的请求后回放 SSE。
func fakeAnthropicUpstream(t *testing.T, gotReq *[]byte, gotUA, gotKey *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*gotReq = body
		*gotUA = r.Header.Get("User-Agent")
		*gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(fakeUpstreamSSE))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGatewayPassthroughAndRecord 端到端：客户端 → 网关 → fake 上游。
// 断言三件事：
//  1. 客户端收到的字节与上游产出完全一致（透传无损）
//  2. 上游收到的请求体/鉴权头与客户端发出的一致（请求透传）
//  3. 账本记录出 agent/model/usage/toolCalls/session，密钥已脱敏
func TestGatewayPassthroughAndRecord(t *testing.T) {
	var gotReq []byte
	var gotUA, gotKey string
	up := fakeAnthropicUpstream(t, &gotReq, &gotUA, &gotKey)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := New(Options{Upstreams: map[string]string{decode.ProtoAnthropic: up.URL}}, st)
	gwSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(gwSrv.Close)

	reqBody := `{"model":"claude-opus-5-5","max_tokens":1024,"stream":true,` +
		`"messages":[{"role":"user","content":"列出目录"}]}`
	req, _ := http.NewRequest("POST", gwSrv.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
	req.Header.Set("X-Api-Key", "sk-ant-test-1234567890")
	req.Header.Set("X-Claude-Code-Session-Id", "sess-e2e")
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	clientBytes, _ := io.ReadAll(resp.Body)

	// 1. 透传无损
	if !bytes.Equal(clientBytes, []byte(fakeUpstreamSSE)) {
		t.Fatalf("透传字节不一致:\n客户端得到 %q\n期望 %q", clientBytes, fakeUpstreamSSE)
	}
	// 2. 请求透传
	if string(gotReq) != reqBody {
		t.Fatalf("上游收到的请求体不对: %s", gotReq)
	}
	if gotUA != "claude-cli/2.1.0 (external, cli)" {
		t.Fatalf("UA 没有透传: %q", gotUA)
	}
	if gotKey != "sk-ant-test-1234567890" {
		t.Fatalf("鉴权头没有透传给上游: %q", gotKey)
	}
	// 3. 账本：客户端读完响应时服务端可能尚未落盘（Append 在最后一个字节之后），
	//    轮询等待最多 2 秒
	var recs []store.Record
	for i := 0; i < 40; i++ {
		recs, err = st.Records(time.Now().Format("2006-01-02"))
		if err == nil && len(recs) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || len(recs) != 1 {
		t.Fatalf("账本应有 1 条: %v %d", err, len(recs))
	}
	rec := recs[0]
	if rec.Agent != "claude-code" || rec.Session != "sess-e2e" {
		t.Errorf("归因不对: %+v", rec)
	}
	if rec.Model != "claude-opus-5-5" || rec.Status != 200 {
		t.Errorf("model/status 不对: %s %d", rec.Model, rec.Status)
	}
	if rec.Usage == nil || rec.Usage.Input != 77 || rec.Usage.Output != 21 || rec.Usage.CacheRead != 33 {
		t.Errorf("usage 不对: %+v", rec.Usage)
	}
	if len(rec.ToolCalls) != 1 || rec.ToolCalls[0].Name != "Bash" {
		t.Errorf("toolCalls 不对: %+v", rec.ToolCalls)
	}
	// body 文件：鉴权头已脱敏
	body, err := st.Body(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := body.RequestHeaders["X-Api-Key"]; !ok || !strings.HasPrefix(v, "sk-ant-tes") || !strings.Contains(v, "*") {
		t.Errorf("X-Api-Key 未脱敏: %q", v)
	}
	if body.Result == nil || body.Result.Text != "我先看看目录" {
		t.Errorf("聚合文本不对: %+v", body.Result)
	}
}

// TestGatewayUpstreamFail 上游不可达：客户端收到 502，账本记录错误。
func TestGatewayUpstreamFail(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	// 指向一个不存在的端口
	gw := New(Options{Upstreams: map[string]string{decode.ProtoAnthropic: "http://127.0.0.1:1"}}, st)
	gwSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(gwSrv.Close)

	resp, err := http.Post(gwSrv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("期望 502，得到 %d", resp.StatusCode)
	}
	recs, _ := st.Records(time.Now().Format("2006-01-02"))
	if len(recs) != 1 || !strings.Contains(recs[0].Error, "上游") {
		t.Fatalf("失败记录不对: %+v", recs)
	}
}

// TestProtoOfPath 协议识别。
func TestProtoOfPath(t *testing.T) {
	cases := map[string]string{
		"/v1/messages":              decode.ProtoAnthropic,
		"/v1/messages/count_tokens": decode.ProtoAnthropic,
		"/v1/chat/completions":      decode.ProtoOpenAIChat,
		"/v1/responses":             decode.ProtoOpenAIResponses,
		"/v1beta/models/x:generateContent": decode.ProtoRaw,
	}
	for path, want := range cases {
		r, _ := http.NewRequest("POST", "http://x"+path, nil)
		if got := protoOfPath(r); got != want {
			t.Errorf("%s → %s，期望 %s", path, got, want)
		}
	}
	// 未识别路径：按鉴权头风格猜测
	r, _ := http.NewRequest("POST", "http://x/v1/embeddings", nil)
	r.Header.Set("X-Api-Key", "k")
	if got := protoOfPath(r); got != decode.ProtoAnthropic {
		t.Errorf("x-api-key 猜测失败: %s", got)
	}
}

// TestGatewayLive 端到端：带 Hub 的网关，订阅者应按序收到
// req_start → 若干 event（含文本增量）→ req_end，三者用同一 ID 关联。
func TestGatewayLive(t *testing.T) {
	var gotReq []byte
	var gotUA, gotKey string
	up := fakeAnthropicUpstream(t, &gotReq, &gotUA, &gotKey)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := live.NewHub()
	gw := New(Options{
		Upstreams: map[string]string{decode.ProtoAnthropic: up.URL},
		Hub:       hub,
	}, st)
	gwSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(gwSrv.Close)

	ch, cancel := hub.Subscribe()
	defer cancel()

	reqBody := `{"model":"claude-opus-5-5","max_tokens":64,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gwSrv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 收集全部消息（req_end 之后的都读出来）
	var msgs []live.Msg
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case m := <-ch:
			msgs = append(msgs, m)
			if m.Type == live.TypeReqEnd {
				break collect
			}
		case <-deadline:
			t.Fatalf("超时：未收到 req_end，共收到 %d 条", len(msgs))
		}
	}

	if len(msgs) < 3 {
		t.Fatalf("消息太少: %+v", msgs)
	}
	id := msgs[0].ID
	if msgs[0].Type != live.TypeReqStart {
		t.Errorf("首条应是 req_start: %+v", msgs[0])
	}
	if msgs[0].Model != "claude-opus-5-5" {
		t.Errorf("req_start 应带 model: %+v", msgs[0])
	}
	var sawText bool
	for _, m := range msgs[1 : len(msgs)-1] {
		if m.Type != live.TypeEvent || m.ID != id {
			t.Fatalf("中间消息不对: %+v", m)
		}
		if m.Ev != nil && m.Ev.Kind == ir.KText {
			sawText = true
		}
	}
	if !sawText {
		t.Error("event 流里应有文本增量")
	}
	last := msgs[len(msgs)-1]
	if last.Type != live.TypeReqEnd || last.Record == nil || last.ID != id {
		t.Fatalf("req_end 不对: %+v", last)
	}
	if last.Record.ID != id || last.Record.Status != 200 {
		t.Errorf("req_end 记录不对: %+v", last.Record)
	}
}

// TestProxyFunc 二级代理策略：非环回目标走上级代理，环回（本地上游）直连。
func TestProxyFunc(t *testing.T) {
	f := proxyFunc("http://127.0.0.1:7897")
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	u, err := f(req)
	if err != nil || u == nil || u.Host != "127.0.0.1:7897" {
		t.Errorf("外网目标应走代理: %v %v", u, err)
	}
	req2, _ := http.NewRequest("POST", "http://127.0.0.1:3425/v1/messages", nil)
	if u, _ := f(req2); u != nil {
		t.Errorf("环回上游应直连: %v", u)
	}
	// 未配置 / 非法配置：退回环境变量语义（不 panic 即可）
	if f2 := proxyFunc(""); f2 == nil {
		t.Error("未配置时应有默认 ProxyFunc")
	}
	if f3 := proxyFunc("://bad url"); f3 == nil {
		t.Error("非法配置时应有默认 ProxyFunc")
	}
}

// TestMITMNoiseFilter MITM（HostUpstream）模式：浏览器杂流量（GET、无 LLM
// 特征的 body）透传但不入账；带 messages 的 POST 正常入账。
func TestMITMNoiseFilter(t *testing.T) {
	var hitPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	upURL := strings.TrimPrefix(up.URL, "http://")

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{HostUpstream: true, InsecureUpstream: true}, st)
	h := s.Handler()

	// 杂流量：GET 心跳（豆包桌面端形状）
	r1 := httptest.NewRequest(http.MethodGet, "http://"+upURL+"/samantha/bookmark/heartbeat", nil)
	r1.Host = upURL
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, r1)
	if w1.Code != 200 || w1.Body.String() != `{"ok":true}` {
		t.Fatalf("杂流量透传失败: %d %s", w1.Code, w1.Body.String())
	}
	if hitPath != "/samantha/bookmark/heartbeat" {
		t.Fatalf("杂流量没到上游: %s", hitPath)
	}

	// 真 LLM 请求：POST 带 messages
	body := `{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":false}`
	r2 := httptest.NewRequest(http.MethodPost, "http://"+upURL+"/v1/chat/completions", strings.NewReader(body))
	r2.Host = upURL
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != 200 {
		t.Fatalf("LLM 请求失败: %d", w2.Code)
	}

	// 账本里只有 LLM 那条
	recs, err := st.Records("")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Path != "/v1/chat/completions" {
		t.Fatalf("杂流量过滤失败，账本: %+v", recs)
	}
}
