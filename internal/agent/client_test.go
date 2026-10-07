package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// echoHandler 返回 200 ok（握手验证用）。
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// TestHTTPSOnly console_url 强制 https（R10-#1）：明文 http 与其他 scheme 一律
// 拒绝；https 且无 ca_cert/fingerprint 拒绝（无跳过验证选项）。
func TestHTTPSOnly(t *testing.T) {
	for _, bad := range []string{
		"http://127.0.0.1:7700",
		"http://localhost/mesh",
		"ftp://127.0.0.1:7700",
		"127.0.0.1:7700",
		"",
	} {
		if _, err := NewHTTPClient(bad, "ca.crt", ""); err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("console_url %q must be rejected with https hint, got %v", bad, err)
		}
	}
	if _, err := NewHTTPClient("https://127.0.0.1:7700", "", ""); err == nil {
		t.Fatal("https without pinning must be rejected")
	}
}

// TestFingerprintPinning 真实握手（httptest 自签证书即下发链末端信任锚）：
// 指纹匹配→成功；篡改→握手失败且错误不含证书材料。
func TestFingerprintPinning(t *testing.T) {
	srv := httptest.NewTLSServer(echoHandler())
	defer srv.Close()

	der := srv.Certificate().Raw
	sum := sha256.Sum256(der)
	goodFP := hex.EncodeToString(sum[:])

	// 指纹匹配：请求成功（链/有效期/主机名验证 + 指纹全部通过）。
	client, err := NewHTTPClient(srv.URL, "", goodFP)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	resp, err := client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("request with correct fingerprint failed: %v", err)
	}
	resp.Body.Close()

	// 冒号分隔大写形式同样接受（与 openssl -fingerprint 输出兼容）。
	var colon strings.Builder
	for i := 0; i < len(goodFP); i++ {
		if i > 0 && i%2 == 0 {
			colon.WriteByte(':')
		}
		c := goodFP[i]
		if c >= 'a' && c <= 'f' {
			c -= 32 // 仅字母转大写，数字保持
		}
		colon.WriteByte(c)
	}
	client2, err := NewHTTPClient(srv.URL, "", colon.String())
	if err != nil {
		t.Fatalf("colon fingerprint rejected: %v", err)
	}
	if resp, err = client2.Get(srv.URL + "/healthz"); err != nil {
		t.Fatalf("colon fingerprint request failed: %v", err)
	}
	resp.Body.Close()

	// 篡改指纹：TLS 错误，错误串不含任何指纹/证书材料。
	badFP := goodFP[:62] + "00"
	client3, err := NewHTTPClient(srv.URL, "", badFP)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	_, err = client3.Get(srv.URL + "/healthz")
	if err == nil {
		t.Fatal("request with tampered fingerprint must fail")
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), goodFP) || strings.Contains(err.Error(), badFP) {
		t.Fatalf("error must not contain fingerprint material: %v", err)
	}
}

// testPKI 生成测试用 CA + 叶子证书。
type testPKI struct {
	caDER, leafDER []byte
	leaf           *x509.Certificate
	leafKey        *ecdsa.PrivateKey
}

func makeTestPKI(t *testing.T, leafIPs []string, leafDNS []string, notAfter time.Time) *testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "meshconsole test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "meshconsole test leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     leafDNS,
	}
	for _, s := range leafIPs {
		if ip := net.ParseIP(s); ip != nil {
			leafTpl.IPAddresses = append(leafTpl.IPAddresses, ip)
		} else {
			t.Fatalf("bad test ip %q", s)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{caDER: caDER, leafDER: leafDER, leaf: leaf, leafKey: leafKey}
}

// chainServer 用给定证书链起一个 TLS 测试服务端（certs 为下发链，叶子在前）。
func chainServer(t *testing.T, p *testPKI, chain [][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(echoHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: chain,
		PrivateKey:  p.leafKey,
	}}}
	srv.StartTLS()
	return srv
}

// TestFingerprintRequiresFullChain 仅指纹模式执行完整 x509 验证（R10-#1）：
// CA 签发的完整链（叶子+CA）→ 通过；只发叶子（无 CA 可建链）→ 指纹虽匹配仍
// 拒绝；过期证书 → 拒绝；主机名不匹配 → 拒绝。
func TestFingerprintRequiresFullChain(t *testing.T) {
	pkiOK := makeTestPKI(t, []string{"127.0.0.1"}, []string{"localhost"}, time.Now().Add(24*time.Hour))
	sumOK := sha256.Sum256(pkiOK.leafDER)
	fp := hex.EncodeToString(sumOK[:])

	// 完整链下发：通过。
	srv := chainServer(t, pkiOK, [][]byte{pkiOK.leafDER, pkiOK.caDER})
	client, err := NewHTTPClient(srv.URL, "", fp)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	if resp, err := client.Get(srv.URL + "/healthz"); err != nil {
		t.Fatalf("full chain must verify: %v", err)
	} else {
		resp.Body.Close()
	}
	srv.Close()

	// 只发叶子：x509 链建不起来（签发方 CA 未下发、叶子非自签）——
	// 指纹匹配也不能通过，证明这不是"仅比对叶子字节"。
	srvLeafOnly := chainServer(t, pkiOK, [][]byte{pkiOK.leafDER})
	client2, err := NewHTTPClient(srvLeafOnly.URL, "", fp)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	_, err = client2.Get(srvLeafOnly.URL + "/healthz")
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("leaf-only chain must fail verification even with matching fingerprint, got %v", err)
	}
	srvLeafOnly.Close()

	// 过期证书：指纹匹配仍拒绝（有效期校验执行）。
	pkiExpired := makeTestPKI(t, []string{"127.0.0.1"}, []string{"localhost"}, time.Now().Add(-time.Hour))
	srvExpired := chainServer(t, pkiExpired, [][]byte{pkiExpired.leafDER, pkiExpired.caDER})
	sumExp := sha256.Sum256(pkiExpired.leafDER)
	fpExp := hex.EncodeToString(sumExp[:])
	client3, err := NewHTTPClient(srvExpired.URL, "", fpExp)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	_, err = client3.Get(srvExpired.URL + "/healthz")
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("expired cert must fail verification, got %v", err)
	}
	srvExpired.Close()

	// 主机名不匹配：叶子 SAN 不含 127.0.0.1，指纹匹配仍拒绝。
	pkiWrongHost := makeTestPKI(t, nil, []string{"example.com"}, time.Now().Add(24*time.Hour))
	srvHost := chainServer(t, pkiWrongHost, [][]byte{pkiWrongHost.leafDER, pkiWrongHost.caDER})
	sumHost := sha256.Sum256(pkiWrongHost.leafDER)
	fpHost := hex.EncodeToString(sumHost[:])
	client4, err := NewHTTPClient(srvHost.URL, "", fpHost)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	_, err = client4.Get(srvHost.URL + "/healthz")
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("hostname mismatch must fail verification, got %v", err)
	}
	srvHost.Close()
}

// TestRedirectDowngradeRejected 重定向到 http 一律拒绝（R10-#1）。
func TestRedirectDowngradeRejected(t *testing.T) {
	pki := makeTestPKI(t, []string{"127.0.0.1"}, []string{"localhost"}, time.Now().Add(24*time.Hour))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/plaintext", http.StatusFound)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{pki.leafDER, pki.caDER},
		PrivateKey:  pki.leafKey,
	}}}
	srv.StartTLS()
	defer srv.Close()

	fp := hex.EncodeToString(func() []byte { s := sha256.Sum256(pki.leafDER); return s[:] }())
	client, err := NewHTTPClient(srv.URL, "", fp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Get(srv.URL + "/somewhere")
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("http redirect must be refused, got %v", err)
	}
}

// TestCACertPinning ca_cert 路径：链验证通过；配 CA 同时改指纹 → 指纹复核仍拦截。
func TestCACertPinning(t *testing.T) {
	pki := makeTestPKI(t, []string{"127.0.0.1"}, []string{"localhost"}, time.Now().Add(24*time.Hour))
	srv := chainServer(t, pki, [][]byte{pki.leafDER, pki.caDER})
	defer srv.Close()

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.caDER}), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := NewHTTPClient(srv.URL, caPath, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	resp, err := client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("request with CA pin failed: %v", err)
	}
	resp.Body.Close()

	// CA 正确但指纹错误：双重校验下指纹仍然拦截（任一失败即拒绝）。
	fp := hex.EncodeToString(func() []byte { s := sha256.Sum256(pki.leafDER); return s[:] }())
	wrongFP := fp[:62] + "ff"
	client2, err := NewHTTPClient(srv.URL, caPath, wrongFP)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	if _, err = client2.Get(srv.URL + "/healthz"); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("wrong fingerprint must fail even with valid CA: %v", err)
	}
}

// TestPinnedTLSConfigBaseline TLS 基线：MinVersion ≥ TLS1.2。
func TestPinnedTLSConfigBaseline(t *testing.T) {
	if _, err := pinnedTLSConfig("127.0.0.1", "", "ab"); err == nil {
		t.Fatal("bad fingerprint must be rejected")
	}
	cfg, err := pinnedTLSConfig("127.0.0.1", "", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("min version = %x, want TLS1.2", cfg.MinVersion)
	}
	// 仅指纹模式：系统链验证被替换为「下发链重建完整验证 + 指纹」（hook 必在，
	// 绝不是裸跳过——链/有效期/主机名/指纹任一失败握手即失败）。
	if !cfg.InsecureSkipVerify || cfg.VerifyPeerCertificate == nil {
		t.Fatalf("fingerprint-only mode must replace verification: skip=%v hook=%v",
			cfg.InsecureSkipVerify, cfg.VerifyPeerCertificate != nil)
	}
	if cfg.ServerName != "127.0.0.1" {
		t.Fatalf("ServerName = %q, want dial host", cfg.ServerName)
	}
	// ca_cert 模式：标准链验证（InsecureSkipVerify=false）+ 指纹/无指纹均合法。
	cfg2, err := pinnedTLSConfig("example.com", "testdata-nonexistent", "")
	if err == nil {
		t.Fatal("missing CA file must be rejected")
	}
	_ = cfg2
	cfg3, err := pinnedTLSConfig("example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.InsecureSkipVerify || cfg3.VerifyPeerCertificate != nil {
		t.Fatalf("ca-less plain config unexpected: %+v", cfg3)
	}
}

// TestVerifyServedChainRejectsImpostorAnchor R13-#1：self-issued（Issuer 名单
// 相同）≠ self-signed。伪造锚复制真 CA 名单（RawIssuer==RawSubject）但 TBS 由
// 异密钥签署——修复前仅比名字即可入信任池，借 Go「叶子即在信任池」的直通捷径
// 架空签名链验证；修复后入池前用锚自身公钥验证其自签名，不符即拒绝。
func TestVerifyServedChainRejectsImpostorAnchor(t *testing.T) {
	// 锚证书内公钥（impostorKey）与实际签署私钥（signingKey）不同源。
	impostorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caName := pkix.Name{CommonName: "meshconsole test CA"}
	anchorTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               caName,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	// 父证书=自身（Issuer 名单与 Subject 相同）、签名来自异密钥 signingKey。
	anchorDER, err := x509.CreateCertificate(rand.Reader, anchorTpl, anchorTpl, &impostorKey.PublicKey, signingKey)
	if err != nil {
		t.Fatal(err)
	}

	// 同 CA 名单但由异密钥签发的叶子（Issuer 名单与锚一致、签名同样来自 signingKey）。
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(8),
		Subject:      pkix.Name{CommonName: "meshconsole test leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, anchorTpl, &leafKey.PublicKey, signingKey)
	if err != nil {
		t.Fatal(err)
	}

	// 场景一：单证书呈现（伪造锚自身充当叶子）——修复前该形态 RawIssuer==
	// RawSubject 即入池，Go 直通捷径令链验证整体放行。
	if leaf, err := verifyServedChain([][]byte{anchorDER}, "localhost"); err == nil {
		t.Fatalf("self-issued but not self-signed anchor must be rejected, got leaf %q", leaf.Subject)
	} else if !strings.Contains(err.Error(), "self-signed") {
		t.Fatalf("unexpected rejection: %v", err)
	}

	// 场景二：同名单异密钥叶子 + 伪造锚的双证书链，同样在锚自签名处拒绝。
	if _, err := verifyServedChain([][]byte{leafDER, anchorDER}, "localhost"); err == nil || !strings.Contains(err.Error(), "self-signed") {
		t.Fatalf("chain behind impostor anchor must be rejected, got %v", err)
	}
}

// TestNormalizeFingerprintNoEcho R13-#3：指纹格式错误的提示只说明 hex 格式
// 要求，不回显原值（错误信息不留任何输入材料片段，与 config 侧同口径）。
func TestNormalizeFingerprintNoEcho(t *testing.T) {
	// 非 hex：64 位但含 zz（非 hex 且不会自然出现在提示文案中）。
	bad := strings.Repeat("ab", 31) + "zz"
	_, err := normalizeFingerprint(bad)
	if err == nil || !strings.Contains(err.Error(), "hex") {
		t.Fatalf("non-hex fingerprint must be rejected with hex hint, got %v", err)
	}
	if strings.Contains(err.Error(), "zz") || strings.Contains(err.Error(), bad) {
		t.Fatalf("error must not echo the original value: %v", err)
	}

	// 长度错误：带独特标记的短输入同样不得回显。
	marker := "topsecret-input"
	_, err = normalizeFingerprint(marker)
	if err == nil {
		t.Fatal("short fingerprint must be rejected")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("length error must not echo the original value: %v", err)
	}

	// 正常路径不受影响：冒号/大小写/空白归一化。
	got, err := normalizeFingerprint(strings.ToUpper(strings.Repeat("ab", 32)))
	if err != nil || got != strings.Repeat("ab", 32) {
		t.Fatalf("valid fingerprint normalize broken: %q %v", got, err)
	}
}

// TestRegisterOverTLS 注册请求经 TLS 固定验证走通（返回合法 JSON 但缺 node_token）。
func TestRegisterOverTLS(t *testing.T) {
	jsonSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","node_id":1,"node_name":"n1"}`))
	}))
	defer jsonSrv.Close()
	sum := sha256.Sum256(jsonSrv.Certificate().Raw)
	client, err := NewHTTPClient(jsonSrv.URL, "", hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// JSON 解析成功但无 node_token → 协议层错误，证明 TLS 层已通。
	_, err = Register(ctx, client, jsonSrv.URL, "reg-token", "n1", "role", "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "node_token") {
		t.Fatalf("expected protocol-level error (TLS already ok), got: %v", err)
	}
	if strings.Contains(err.Error(), "tls:") {
		t.Fatalf("unexpected TLS error: %v", err)
	}
}
