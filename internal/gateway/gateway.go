// Package gateway 本地透传网关：agent 把 base_url 指到这里，网关把请求
// 原样转发到真实上游，同时旁路录制完整请求/响应/流式事件。
//
// 第一原则：透传路径上的一切（解码、归因、落盘）失败都不影响请求本身。
package gateway

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ai-trace/internal/attrib"
	"ai-trace/internal/decode"
	"ai-trace/internal/ir"
	"ai-trace/internal/live"
	"ai-trace/internal/skills"
	"ai-trace/internal/store"
)

// proxyFunc 上级代理策略：配置了 Proxy 时非环回目标走它，环回目标（本地
// magpie 等上游）直连；未配置时退回环境变量（HTTP_PROXY/HTTPS_PROXY/NO_PROXY）。
func proxyFunc(proxyURL string) func(*http.Request) (*url.URL, error) {
	if proxyURL == "" {
		return http.ProxyFromEnvironment
	}
	u, err := url.Parse(proxyURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
		return http.ProxyFromEnvironment // 配置非法，退回环境变量
	}
	return func(req *http.Request) (*url.URL, error) {
		if isLoopbackHost(req.URL.Hostname()) {
			return nil, nil // 本地上游直连，不绕代理
		}
		return u, nil
	}
}

// isLoopbackHost 判断 host 是否环回（127.x / ::1 / localhost）。
func isLoopbackHost(host string) bool {
	if host == "localhost" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// 默认上游地址
const (
	UpstreamAnthropic = "https://api.anthropic.com"
	UpstreamOpenAI    = "https://api.openai.com"
	UpstreamGemini    = "https://generativelanguage.googleapis.com"
)

// maxBodyBytes 请求体读取上限（长上下文请求可达数 MB，32MiB 足够宽裕）
const maxBodyBytes = 32 << 20

// maxCaptureBytes 响应原文录制上限（超出截断并标记，聚合结果不受影响）
const maxCaptureBytes = 16 << 20

// Options 网关配置。
type Options struct {
	Addr      string            // 监听地址，默认 127.0.0.1:8787
	Upstreams map[string]string // 协议 -> 上游 base url 覆盖（测试/私有化部署用）
	// HostUpstream 按请求 Host 决定上游（MITM 模式用：CONNECT 目标域就是真实上游，
	// 不能按协议默认值转发，否则 api.deepseek.com 的流量会被发到 api.anthropic.com）
	HostUpstream bool
	// InsecureUpstream 不校验上游 TLS 证书（仅测试用自签上游时开启）
	InsecureUpstream bool
	// Hub 实时事件总线（可为 nil：不推送，行为与旧版一致）
	Hub *live.Hub
	// Proxy 上级代理（二级代理：MITM 解密录制后经它出网，如本机 Clash）。
	// 环回目标（本地 magpie 等上游）不走它，直连。
	Proxy string
}

// Server 网关服务。
type Server struct {
	opts   Options
	store  *store.Store
	client *http.Client
}

// New 创建网关。st 为已打开的存储。
func New(opts Options, st *store.Store) *Server {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:8787"
	}
	tr := &http.Transport{
		// 关键：禁用透明 gzip 解压。Go 默认会解压上游响应再给到我们，
		// 但响应头里仍带 Content-Encoding: gzip，客户端就会二次解压失败。
		// 禁用后字节原样透传，录制到的也是线上真实字节。
		DisableCompression: true,
		Proxy:              proxyFunc(opts.Proxy),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: opts.InsecureUpstream, // 仅测试用
		},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute, // 深度思考可能很久才出首字节
	}
	return &Server{
		opts:  opts,
		store: st,
		// 不设总超时：流式响应时长不可控
		client: &http.Client{Transport: tr},
	}
}

// Addr 监听地址。
func (s *Server) Addr() string { return s.opts.Addr }

// protoOfPath 按请求路径识别协议；辅以鉴权头风格猜测。
func protoOfPath(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/v1/messages"):
		return decode.ProtoAnthropic
	case strings.HasPrefix(p, "/v1/chat/completions"):
		return decode.ProtoOpenAIChat
	case strings.HasPrefix(p, "/v1/responses"):
		return decode.ProtoOpenAIResponses
	case strings.HasPrefix(p, "/chat/completion"):
		return decode.ProtoDoubao // 豆包 samantha（www.doubao.com）
	case strings.HasPrefix(p, "/v1beta/"):
		return decode.ProtoRaw // gemini：Phase 1 先原文录制
	default:
		// 未识别路径按鉴权头风格兜底猜测，尽量让边缘端点也能透传
		if r.Header.Get("X-Api-Key") != "" {
			return decode.ProtoAnthropic
		}
		return decode.ProtoOpenAIChat
	}
}

// upstreamFor 协议对应的默认上游。
func upstreamFor(proto string) string {
	switch proto {
	case decode.ProtoAnthropic:
		return UpstreamAnthropic
	case decode.ProtoOpenAIChat, decode.ProtoOpenAIResponses:
		return UpstreamOpenAI
	case decode.ProtoDoubao:
		return "https://www.doubao.com"
	default:
		return UpstreamGemini
	}
}

// upstreamFor 协议对应的上游（Options.Upstreams 优先）。
func (s *Server) upstreamFor(proto string) string {
	if u, ok := s.opts.Upstreams[proto]; ok {
		return u
	}
	return upstreamFor(proto)
}

// hopHeaders 逐跳头，转发时剥掉（RFC 7230）。
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Handler 网关透传处理器（挂 /v1/ 与 /v1beta/ 路径）。
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	proto := protoOfPath(r)
	id := store.NewID() // 提前生成：实时推送与最终落盘用同一 ID 关联

	// 1. 读请求体
	var reqBody []byte
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		var err error
		reqBody, err = io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			http.Error(w, "读请求体失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		if int64(len(reqBody)) > maxBodyBytes {
			http.Error(w, "请求体超过 32MiB 上限", http.StatusRequestEntityTooLarge)
			return
		}
	}

	// WorkBuddy CLI 等客户端会把请求体 gzip 压缩（Content-Encoding: gzip）
	// 后发送：解压一份仅用于旁路分析与录制，转发仍用原始字节，透传无损。
	analyzed := reqBody
	if isGzipBody(r.Header.Get("Content-Encoding"), reqBody) {
		if raw, ok := gunzip(reqBody); ok {
			analyzed = raw
		}
	}

	// 2. 旁路分析（归因、model 提取）
	info := attrib.Analyze(r, analyzed, proto)
	model := extractModel(analyzed)

	// MITM 模式下系统代理会把客户端的全部流量送进来：会话同步、遥测上报、
	// 资源下载等与 LLM 无关的请求不入账，只透传——否则账本被后台 API 灌满、
	// 回放器里全是"没有 messages"的记录。统一按请求形状判定（不因 agent
	// 已识别而放宽——WorkBuddy 等客户端的后台 API 也带着自家 UA）：
	// POST 类 + JSON 对象 + 顶层有 LLM 请求特征字段，三样齐才录。
	if s.opts.HostUpstream && !worthRecording(r.Method, analyzed) {
		s.passthroughOnly(w, r, reqBody)
		return
	}

	// 技能沉淀：请求里注入的技能/专家包正文（<expert_selection> 等）解析后
	// 落盘到 <dataDir>/skills/。纯旁路，失败即弃，不影响请求。
	skills.Capture(s.store.Root(), analyzed, skills.Meta{
		Agent: info.Agent, Session: info.Session, Model: model,
	})

	// 实时推送上下文（hub 为空则全部为 no-op）
	lctx := &liveCtx{hub: s.opts.Hub, base: live.Msg{
		ID: id, Agent: info.Agent, Session: info.Session, Subagent: info.Subagent,
		Model: model, Proto: proto, Path: r.URL.Path,
	}}
	lctx.publish(live.Msg{Type: live.TypeReqStart, Method: r.Method})

	// 3. 构造上游请求
	upBase := s.upstreamFor(proto)
	if s.opts.HostUpstream && r.Host != "" {
		// MITM 模式：请求的 Host 就是客户端 CONNECT 的真实目标域，
		// 必须按它转发（否则第三方 API 域的流量会被错发到协议默认上游）
		scheme := "https"
		if r.URL.Scheme == "http" {
			scheme = "http"
		}
		upBase = scheme + "://" + r.Host
	}
	upURL := upBase + r.URL.Path
	if r.URL.RawQuery != "" {
		upURL += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "构造上游请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			upReq.Header.Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		upReq.Header.Del(h)
	}
	upReq.ContentLength = int64(len(reqBody))

	// 4. 发上游
	resp, err := s.client.Do(upReq)
	if err != nil {
		rec := store.Record{
			ID: id, Time: start.UTC(), Agent: info.Agent, Session: info.Session,
			Subagent: info.Subagent, Protocol: proto, Method: r.Method, Path: r.URL.Path,
			Model: model, DurationMS: msSince(start), ReqBytes: int64(len(reqBody)),
			Error: "上游连接失败: " + err.Error(),
		}
		s.store.Append(rec, store.FullBody{
			RequestHeaders:  redactHeaders(r.Header),
			RequestBody:     json.RawMessage(analyzed),
			ResponseHeaders: map[string]string{},
		})
		lctx.publish(live.Msg{Type: live.TypeReqEnd, Record: &rec})
		http.Error(w, "ai-trace: 上游不可达: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 5. 复制响应头给客户端
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)

	// 6. 流式透传 + 旁路录制
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	rec := newRecorder(proto, isSSE, lctx)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var respAll []byte
	truncated := false
	ttft := int64(0)
	clientGone := false
	var writeErr error

	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if ttft == 0 {
				ttft = msSince(start)
			}
			chunk := buf[:n]
			// 先录制后写客户端：客户端断开也要把上游读完录完
			if len(respAll) < maxCaptureBytes {
				remain := maxCaptureBytes - len(respAll)
				if len(chunk) > remain {
					respAll = append(respAll, chunk[:remain]...)
					truncated = true
				} else {
					respAll = append(respAll, chunk...)
				}
			} else {
				truncated = true
			}
			rec.feed(chunk)
			if !clientGone {
				if _, err := w.Write(chunk); err != nil {
					clientGone = true // 客户端断开：停止写，继续读上游录完
					writeErr = err
				} else if flusher != nil && isSSE {
					flusher.Flush() // SSE 必须及时冲刷
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				rec.err(rerr)
			}
			break
		}
	}
	rec.flush()
	if truncated {
		respAll = append(respAll, []byte("\n[ai-trace: 响应超过录制上限，已截断]")...)
	}

	// 7. 落盘（错误由 store 内部吞掉）
	res := rec.result()
	rec2 := store.Record{
		ID: id, Time: start.UTC(), Agent: info.Agent, Session: info.Session,
		Subagent: info.Subagent, Protocol: proto, Method: r.Method, Path: r.URL.Path,
		Status: resp.StatusCode, Model: model, Usage: res.Usage,
		StopReason: res.StopReason, ToolCalls: res.ToolCalls,
		DurationMS: msSince(start), TTFTMS: ttft,
		ReqBytes: int64(len(reqBody)), RespBytes: int64(len(respAll)),
		Error: recErrString(rec, writeErr, clientGone),
	}
	s.store.Append(rec2, store.FullBody{
		RequestHeaders:  redactHeaders(r.Header),
		RequestBody:     json.RawMessage(analyzed),
		ResponseHeaders: redactHeaders(resp.Header),
		ResponseBody:    string(respAll),
		Result:          &res,
	})
	lctx.publish(live.Msg{Type: live.TypeReqEnd, Record: &rec2})
}

// isGzipBody 请求体是否 gzip 压缩：头部声明或 gzip 魔数（1f 8b）。
func isGzipBody(enc string, body []byte) bool {
	if strings.Contains(strings.ToLower(enc), "gzip") {
		return true
	}
	return len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b
}

// gunzip 解压请求体的分析副本；超限或损坏返回 false（透传不受影响）。
func gunzip(body []byte) ([]byte, bool) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, maxBodyBytes+1))
	if err != nil || int64(len(raw)) > maxBodyBytes {
		return nil, false
	}
	return raw, true
}

// worthRecording MITM 模式的入账门槛：POST/PUT/PATCH + JSON 对象 +
// 顶层含 LLM 请求特征字段（messages/model/input/contents/stream/prompt）。
// 网关（base_url）模式不走这里——agent 只会把 LLM 请求发过来，全录。
func worthRecording(method string, body []byte) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	if len(body) == 0 || body[0] != '{' {
		return false // JSON 数组（遥测批量上报）与其他形状不录
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	for _, k := range []string{"messages", "model", "input", "contents", "stream", "prompt"} {
		if _, ok := probe[k]; ok {
			return true
		}
	}
	return false
}

// passthroughOnly 不入账的纯透传：构造上游请求转发，字节原样返回。
// 与 serve 主路径的差异：不落盘、不推实时事件、不耗记录 id。
func (s *Server) passthroughOnly(w http.ResponseWriter, r *http.Request, reqBody []byte) {
	upBase := ""
	if r.Host != "" {
		scheme := "https"
		if r.URL.Scheme == "http" {
			scheme = "http"
		}
		upBase = scheme + "://" + r.Host
	} else {
		upBase = upstreamFor(protoOfPath(r))
	}
	upURL := upBase + r.URL.Path
	if r.URL.RawQuery != "" {
		upURL += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "ai-trace: 构造上游请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			upReq.Header.Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		upReq.Header.Del(h)
	}
	upReq.ContentLength = int64(len(reqBody))

	resp, err := s.client.Do(upReq)
	if err != nil {
		http.Error(w, "ai-trace: 上游不可达: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return // 客户端断开，无需继续
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// liveCtx 一次请求的实时推送上下文（hub 为空则全部 no-op）。
type liveCtx struct {
	hub  *live.Hub
	base live.Msg // 预填 ID/Agent/Model 等公共字段
}

// publish 补齐公共字段后广播。
func (c *liveCtx) publish(m live.Msg) {
	if c == nil || c.hub == nil {
		return
	}
	m.ID, m.Agent, m.Session, m.Subagent = c.base.ID, c.base.Agent, c.base.Session, c.base.Subagent
	m.Model, m.Proto, m.Path = c.base.Model, c.base.Proto, c.base.Path
	c.hub.Publish(m)
}

// recorder 旁路录制状态机：喂入响应字节，产出 IR 事件并聚合。
type recorder struct {
	isSSE bool
	dec   decode.Decoder
	coll  *ir.Collector
	scan  *decode.Scanner
	buf   []byte // 非流式响应的累积缓冲
	res   ir.Result
	errs  []string
	lctx  *liveCtx // 实时推送（可为 nil）
}

func newRecorder(proto string, isSSE bool, lctx *liveCtx) *recorder {
	r := &recorder{isSSE: isSSE, dec: decode.New(proto), coll: ir.NewCollector(), lctx: lctx}
	if isSSE {
		r.scan = &decode.Scanner{}
	}
	return r
}

// feed 喂一段响应字节。
func (r *recorder) feed(chunk []byte) {
	if r.isSSE {
		for _, f := range r.scan.Write(chunk) {
			r.dec.FeedFrame(f, r.emit)
		}
	} else {
		r.buf = append(r.buf, chunk...)
	}
}

// flush 响应流结束：冲刷残余帧并聚合。
func (r *recorder) flush() {
	if r.isSSE {
		for _, f := range r.scan.Flush() {
			r.dec.FeedFrame(f, r.emit)
		}
	} else if len(r.buf) > 0 {
		r.dec.FeedBody(r.buf, r.emit)
	}
	r.res = r.coll.Seal()
}

func (r *recorder) emit(e ir.Event) {
	if e.Kind == ir.KError && e.Err != "" {
		r.errs = append(r.errs, e.Err)
	}
	r.coll.OnEvent(e)
	// 实时推送：每个 IR 事件都广播（token 级增量，本地 localhost 无压力）
	if r.lctx != nil {
		ev := e
		r.lctx.publish(live.Msg{Type: live.TypeEvent, Ev: &ev})
	}
}

// err 记录流读取错误。
func (r *recorder) err(err error) { r.errs = append(r.errs, err.Error()) }

func (r *recorder) result() ir.Result { return r.res }

// recErrString 汇总记录错误字段。
func recErrString(r *recorder, writeErr error, clientGone bool) string {
	parts := append([]string{}, r.errs...)
	if writeErr != nil && clientGone {
		parts = append(parts, "客户端提前断开")
	}
	return strings.Join(parts, "; ")
}

// extractModel 从请求体提取顶层 model 字段（各协议都有）。
func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m struct {
		Model     string `json:"model"`
		ModelName string `json:"model_name"` // WorkBuddy agenttool（如 webfetch）用 model_name
	}
	if json.Unmarshal(body, &m) == nil {
		if m.Model != "" {
			return m.Model
		}
		return m.ModelName
	}
	return ""
}

// redactHeaders header 脱敏后转 map（密钥只保留前缀）。
// Cookie 同样打码：浏览器/App 流量经 MITM 进来时带着完整会话凭据
// （Google SID 等），明文落盘违背"密钥脱敏存储"的承诺。
func redactHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		v := strings.Join(vs, ", ")
		if lk := strings.ToLower(k); lk == "authorization" || lk == "x-api-key" ||
			lk == "cookie" || strings.Contains(lk, "token") || strings.Contains(lk, "key") || strings.Contains(lk, "secret") {
			v = redactSecret(v)
		}
		out[k] = v
	}
	return out
}

// redactSecret 保留前 12 个字符，其余打码。
func redactSecret(s string) string {
	if len(s) <= 12 {
		return strings.Repeat("*", len(s))
	}
	return s[:12] + strings.Repeat("*", 6)
}

func msSince(t time.Time) int64 { return time.Since(t).Milliseconds() }

var _ = fmt.Sprintf // fmt 当前未用，占位防止 import 报错；后续扩展会用到
