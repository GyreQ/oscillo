// Package mitm 入口 B：HTTPS CONNECT 代理（MITM）。
// 对不支持自定义 base_url 的客户端（桌面端等），设 HTTPS_PROXY 指到这里，
// 动态签发目标域证书解出明文，再复用网关的旁路录制管线。
//
// 面向客户端的证书由本地 CA 签发；CA 未被客户端信任时握手会失败，
// 由 capture 命令负责引导（环境变量优先，其次系统信任链）。
package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA 本地根证书（生成一次，存 ~/.ai-trace/ca/）。
type CA struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	mu     sync.Mutex
	leafMu sync.Mutex
	leaves map[string]*tls.Certificate // 域名 -> 签发的叶子证书缓存
}

// caPaths CA 文件位置。
func caPaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

// LoadCA 加载或生成 CA。dir 通常为 ~/.ai-trace/ca。
func LoadCA(dir string) (*CA, error) {
	certPath, keyPath := caPaths(dir)
	if certPEM, errCert := os.ReadFile(certPath); errCert == nil {
		keyPEM, errKey := os.ReadFile(keyPath)
		if errKey == nil {
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err == nil {
				leaf, err := x509.ParseCertificate(cert.Certificate[0])
				if err == nil {
					return &CA{cert: leaf, key: cert.PrivateKey.(*ecdsa.PrivateKey), leaves: map[string]*tls.Certificate{}}, nil
				}
			}
		}
	}
	// 生成新 CA
	ca := &CA{leaves: map[string]*tls.Certificate{}}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "ai-trace CA", Organization: []string{"ai-trace"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	ca.cert = cert
	ca.key = key

	// 落盘（0644 证书可读给环境变量引用；key 0600）
	_ = os.MkdirAll(dir, 0o755)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	return ca, nil
}

// CertPEM CA 证书 PEM（引导安装用）。
func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// CertPath/keyPath 由 LoadCA 调用方传目录，这里给 dashbooard 引导页用得到时再加。

// Leaf 为一个域名签发叶子证书（带缓存）。host 可以是域名或 IP。
func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	if !ca.cert.NotAfter.After(time.Now().Add(24 * time.Hour)) {
		return nil, errors.New("CA 已过期，请删除 ~/.ai-trace/ca 后重试")
	}
	ca.leafMu.Lock()
	defer ca.leafMu.Unlock()
	if c, ok := ca.leaves[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// host 是 IP 时用 IP SAN，否则 DNS SAN（客户端对两者的校验不同）
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	leafCert := &tls.Certificate{
		Certificate: [][]byte{der, ca.cert.Raw}, // 叶子 + CA 链
		PrivateKey:  key,
	}
	ca.leaves[host] = leafCert
	return leafCert, nil
}

// TLSConfig 面向客户端的 TLS 配置（按 SNI 动态签发）。
func (ca *CA) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := hello.ServerName
			if host == "" {
				host = "unknown.local"
			}
			return ca.Leaf(host)
		},
		MinVersion: tls.VersionTLS12,
	}
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 120)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(1)
	}
	return n
}

// hostNoPort 去掉地址里的端口。
func hostNoPort(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
