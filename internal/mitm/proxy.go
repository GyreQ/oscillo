package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ProxyOptions MITM 代理配置。
type ProxyOptions struct {
	Addr    string       // 监听地址，默认 127.0.0.1:8788
	CA      *CA          // 签发证书的本地 CA
	Handler http.Handler // 解出明文后的处理器（传 gateway 的 Handler，开 HostUpstream）
	// DecryptHosts 需要解密录制的域名后缀（小写，如 "anthropic.com"）。
	// 空 = 全部解密（旧行为）；命中才 MITM，其余 CONNECT 隧道直通不解密——
	// 系统代理模式下浏览器等无关流量零影响。
	DecryptHosts []string
	// OutboundProxy 出站上级代理（二级代理）。隧道直通与解密后的出站都经它。
	OutboundProxy string
}

// shouldDecrypt 该 CONNECT 目标是否解密录制。
func (p *Proxy) shouldDecrypt(host string) bool {
	hs := strings.ToLower(host)
	for _, d := range p.opts.DecryptHosts {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if hs == d || strings.HasSuffix(hs, "."+d) {
			return true
		}
	}
	return false
}

// Proxy HTTPS CONNECT 代理。
type Proxy struct {
	opts ProxyOptions
}

// NewProxy 创建代理。
func NewProxy(opts ProxyOptions) *Proxy {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:8788"
	}
	return &Proxy{opts: opts}
}

// Addr 监听地址。
func (p *Proxy) Addr() string { return p.opts.Addr }

// ListenAndServe 阻塞监听。
func (p *Proxy) ListenAndServe() error {
	ln, err := net.Listen("tcp", p.opts.Addr)
	if err != nil {
		return err
	}
	return p.Serve(ln)
}

// Serve 在给定 listener 上服务（测试用 :0 端口时由此拿到实际地址）。
func (p *Proxy) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           http.HandlerFunc(p.serve),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return srv.Serve(ln)
}

// serve 两类请求：CONNECT（TLS MITM）与普通代理请求（明文 http 上游）。
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// 明文代理请求：直接交给录制处理器（如 http:// 上游）
	p.opts.Handler.ServeHTTP(w, r)
}

// handleConnect CONNECT 隧道：命中解密列表的做 MITM，其余原样隧道直通
// （不解密零影响，出站走上级代理保持原网络拓扑）。
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Host
	if target == "" {
		target = r.Host
	}
	if len(p.opts.DecryptHosts) > 0 && !p.shouldDecrypt(hostNoPort(target)) {
		host := hostNoPort(target)
		// 直通日志：定位"白名单漏了哪个 LLM 域名"的线索（一眼看出漏网域名）
		fmt.Fprintf(os.Stderr, "[ai-trace] 直通不解密: %s\n", host)
		p.tunnel(w, r, target)
		return
	}
	p.mitm(w, r, target)
}

// tunnel 不解密的 CONNECT：经上级代理（或直连）打通到目标后双向复制。
// 客户端视角与普通代理完全一致，无证书、无延迟惩罚。
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, target string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 hijack", http.StatusInternalServerError)
		return
	}
	raw, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer raw.Close()

	up, err := p.dialUpstream(r.Context(), target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ai-trace] 隧道连接失败（%s）: %v\n", target, err)
		raw.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer up.Close()
	if _, err := raw.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// hijack 预读的数据也要搬过去
	if buf != nil && buf.Reader.Buffered() > 0 {
		pre, _ := io.ReadAll(buf.Reader)
		if len(pre) > 0 {
			up.Write(pre)
		}
	}
	// 双向复制，任一方向结束即断
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, raw); done <- struct{}{} }()
	go func() { io.Copy(raw, up); done <- struct{}{} }()
	<-done
}

// dialUpstream 拨目标：配了上级代理时向它发 CONNECT 建隧道，否则直连。
func (p *Proxy) dialUpstream(ctx context.Context, target string) (net.Conn, error) {
	proxy := p.opts.OutboundProxy
	if proxy == "" {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	pu, err := url.Parse(proxy)
	if err != nil || pu.Host == "" {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", pu.Host)
	if err != nil {
		return nil, err
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("上级代理拒绝 CONNECT %s: %d", target, resp.StatusCode)
	}
	return c, nil
}

// mitm 解密路径：动态证书解 TLS，明文流量走录制处理器。
// 转发到真实上游由录制处理器完成（gateway 的 HostUpstream 模式按 Host 转发）。
func (p *Proxy) mitm(w http.ResponseWriter, r *http.Request, target string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 hijack", http.StatusInternalServerError)
		return
	}
	raw, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer raw.Close()

	// 确认 CONNECT 建立
	if _, err := raw.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	// 客户端侧 TLS：CONNECT 目标已知，直接预签该域证书（IP 目标不发 SNI，
	// GetCertificate 的 hello.ServerName 会是空，所以这里显式放入 Certificates）
	host := hostNoPort(target)
	if host == "" {
		host = hostNoPort(r.Host)
	}
	leaf, err := p.opts.CA.Leaf(host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ai-trace] 签发证书失败（%s）: %v\n", host, err)
		return
	}
	tlsCfg := p.opts.CA.TLSConfig()
	tlsCfg.Certificates = []tls.Certificate{*leaf}
	tlsConn := tls.Server(newBufferedConn(raw, buf), tlsCfg)
	if err := tlsConn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}
	if err := tlsConn.Handshake(); err != nil {
		// 最常见原因：客户端不信任我们的 CA
		fmt.Fprintf(os.Stderr, "[ai-trace] TLS 握手失败（%s，多半是客户端未信任 ai-trace CA）: %v\n", host, err)
		return
	}
	tlsConn.SetDeadline(time.Time{})

	// 明文 HTTP 交给录制处理器（每条 TLS 连接一个内层 server）
	wc := &watchConn{Conn: tlsConn, done: make(chan struct{})}
	inner := &http.Server{
		Handler:           p.opts.Handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := inner.Serve(&singleListener{conn: wc}); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[ai-trace] 内层 HTTP 服务退出（%s）: %v\n", host, err)
	}
}

// watchConn 在连接关闭时发信号（让 singleListener 的 Accept 知道何时退出）。
type watchConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *watchConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// singleListener 把已建立的连接包装成单次 Accept 的 Listener。
// 第一次 Accept 返回连接；后续 Accept 阻塞到连接关闭才返回 ErrClosed——
// 这样 http.Server.Serve 会一直等服务完这条连接上的 keep-alive 请求，
// 而不是在第二次 Accept 时立刻退出（那会让外层 defer 关掉在用的连接）。
type singleListener struct {
	conn *watchConn
	gave bool
}

func (l *singleListener) Accept() (net.Conn, error) {
	if !l.gave {
		l.gave = true
		return l.conn, nil
	}
	<-l.conn.done
	return nil, net.ErrClosed
}

func (l *singleListener) Close() error   { return nil }
func (l *singleListener) Addr() net.Addr { return l.conn.RemoteAddr() }

// bufferedConn 先读完 hijack 时 bufio 里预读的数据（TLS ClientHello 可能已到达）。
type bufferedConn struct {
	net.Conn
	pre []byte
}

func newBufferedConn(c net.Conn, b *bufio.ReadWriter) *bufferedConn {
	var pre []byte
	if b != nil && b.Reader.Buffered() > 0 {
		pre, _ = io.ReadAll(b.Reader)
	}
	return &bufferedConn{Conn: c, pre: pre}
}

// Read 先读预缓冲，再读底层连接。
func (c *bufferedConn) Read(p []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(p, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
