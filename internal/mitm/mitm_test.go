package mitm

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai-trace/internal/gateway"
	"ai-trace/internal/store"
)

// TestCACertChain CA 生成 → 叶子签发 → 用 CA 验证叶子证书链。
func TestCACertChain(t *testing.T) {
	ca, err := LoadCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	// 解析叶子并验证签名链
	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.anthropic.com"}); err != nil {
		t.Fatalf("证书链验证失败: %v", err)
	}
	// 缓存：同域第二次拿到同一张
	leaf2, _ := ca.Leaf("api.anthropic.com")
	if leaf != leaf2 {
		t.Fatal("叶子证书未缓存")
	}
}

// TestCALoadPersist CA 落盘后重新加载是同一张。
func TestCALoadPersist(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ca1.cert.Equal(ca2.cert) {
		t.Fatal("重新加载的 CA 不是同一张")
	}
	// 密钥文件权限
	fi, _ := os.Stat(filepath.Join(dir, "key.pem"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("CA 私钥权限应为 0600: %v", fi.Mode())
	}
}

// TestMITMEndToEnd MITM 端到端：
// 客户端（信任 ai-trace CA + 走代理）→ MITM 代理 → 自签 TLS 上游。
// 断言：客户端拿到正确响应；账本录到请求（按 CONNECT 目标域转发，非默认上游）。
func TestMITMEndToEnd(t *testing.T) {
	// 1. 自签 TLS 上游（响应 JSON）
	upSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "hello") {
			t.Errorf("上游收到的请求体不对: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"world"}],"usage":{"input_tokens":9,"output_tokens":3}}`))
	}))
	defer upSrv.Close()
	upURL, _ := url.Parse(upSrv.URL)
	upHost := upURL.Host // 127.0.0.1:随机端口

	// 2. MITM 代理：CA + HostUpstream 网关（上游不校验，因测试上游自签）
	st, _ := store.Open(t.TempDir())
	ca, err := LoadCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gateway.Options{HostUpstream: true, InsecureUpstream: true}, st)
	proxy := NewProxy(ProxyOptions{Addr: "127.0.0.1:0", CA: ca, Handler: gw.Handler()})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = proxy.Serve(ln) }()
	proxyAddr := ln.Addr().String()

	// 3. 客户端：信任 CA + 走 CONNECT 代理
	caPool := x509.NewCertPool()
	caPool.AddCert(ca.cert)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: proxyAddr}),
			TLSClientConfig: &tls.Config{RootCAs: caPool},
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Post("https://"+upHost+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"test-m","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("MITM 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "world") {
		t.Fatalf("客户端响应不对: %s", body)
	}

	// 4. 账本：录到了这条 MITM 请求（按 CONNECT 目标域转发成功才会 200）
	var recs []store.Record
	for i := 0; i < 40; i++ {
		recs, _ = st.Records(time.Now().Format("2006-01-02"))
		if len(recs) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(recs) != 1 {
		t.Fatalf("账本应有 1 条 MITM 记录: %d", len(recs))
	}
	if recs[0].Status != 200 || recs[0].Model != "test-m" {
		t.Errorf("MITM 记录不对: %+v", recs[0])
	}
	if recs[0].Protocol != "anthropic" || recs[0].Path != "/v1/messages" {
		t.Errorf("协议识别不对: %+v", recs[0])
	}
}

// TestShouldDecrypt 解密域名匹配：后缀匹配、大小写、非命中直通。
func TestShouldDecrypt(t *testing.T) {
	p := NewProxy(ProxyOptions{DecryptHosts: []string{"anthropic.com", "Copilot.Tencent.com"}})
	cases := map[string]bool{
		"api.anthropic.com":    true,
		"ANTHROPIC.com":        true, // 大小写不敏感
		"anthropic.com":        true, // 裸域
		"copilot.tencent.com":  true,
		"evilanthropic.com":    false, // 不能前缀乱匹配
		"www.google.com":       false,
		"":                     false,
	}
	for host, want := range cases {
		if got := p.shouldDecrypt(host); got != want {
			t.Errorf("shouldDecrypt(%q) = %v，期望 %v", host, got, want)
		}
	}
	// 空列表：shouldDecrypt 恒 false，但 handleConnect 的外层分支
	// （len==0 时不走 tunnel）保证全解密旧行为——这里只验证纯函数语义
	p2 := NewProxy(ProxyOptions{})
	if p2.shouldDecrypt("anything.example") {
		t.Error("空列表纯匹配应返回 false")
	}
}
