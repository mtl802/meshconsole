package pki

import (
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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func ensure(t *testing.T, cfg Config) *Result {
	t.Helper()
	res, err := Ensure(cfg)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return res
}

func readAll(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, f := range []string{CAKeyFile, CACertFile, ServerKeyFile, ServerCertFile} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		out[f] = b
	}
	return out
}

func TestEnsureGeneratesAllFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	res := ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})

	if !res.CAGenerated || !res.ServerCertRenewed {
		t.Fatalf("first ensure should generate CA+cert: %+v", res)
	}
	files := readAll(t, dir)
	_ = files

	// 私钥 0600，证书 0644。
	for _, f := range []string{CAKeyFile, ServerKeyFile} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s perm = %o, want 600", f, perm)
		}
	}
	for _, f := range []string{CACertFile, ServerCertFile} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o644 {
			t.Fatalf("%s perm = %o, want 644", f, perm)
		}
	}

	// 服务端证书：链可验证到 CA，SAN 覆盖 localhost/主机名/127.0.0.1/::1/tailnet IP。
	cert := parseCert(t, files[ServerCertFile])
	caCert := parseCert(t, files[CACertFile])
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("server cert not signed by CA: %v", err)
	}
	host, _ := os.Hostname()
	wantDNS := map[string]bool{"localhost": false, host: false}
	for _, d := range cert.DNSNames {
		if _, ok := wantDNS[d]; ok {
			wantDNS[d] = true
		}
	}
	for d, ok := range wantDNS {
		if !ok {
			t.Fatalf("SAN dns %q missing, got %v", d, cert.DNSNames)
		}
	}
	gotIP := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		gotIP[ip.String()] = true
	}
	for _, want := range []string{"127.0.0.1", "::1", "100.64.0.9"} {
		if !gotIP[want] {
			t.Fatalf("SAN ip %q missing, got %v", want, gotIP)
		}
	}
	if cert.IsCA || !caCert.IsCA {
		t.Fatalf("IsCA wrong: server=%v ca=%v", cert.IsCA, caCert.IsCA)
	}

	// 指纹 = 证书 DER 的 SHA-256（hex 小写）。
	sum := sha256.Sum256(cert.Raw)
	if res.Fingerprint != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint mismatch: %s", res.Fingerprint)
	}

	// ServerTLSConfig 能加载并可用于握手（证书与私钥匹配）。
	tlsCfg, err := ServerTLSConfig(filepath.Join(dir, ServerCertFile), filepath.Join(dir, ServerKeyFile), "")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	if len(tlsCfg.Certificates) != 1 || tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("tls config wrong: %+v", tlsCfg)
	}

	// 指向 ca.crt 时：CA 追加进下发链（供仅指纹 agent 做完整 x509 验证，R10-#1）。
	tlsCfg2, err := ServerTLSConfig(filepath.Join(dir, ServerCertFile), filepath.Join(dir, ServerKeyFile), filepath.Join(dir, CACertFile))
	if err != nil {
		t.Fatalf("ServerTLSConfig with ca: %v", err)
	}
	if n := len(tlsCfg2.Certificates[0].Certificate); n != 2 {
		t.Fatalf("served chain len = %d, want 2 (leaf + CA)", n)
	}
	// CA 文件缺失：保持原样不报错（外部证书管理场景）。
	tlsCfg3, err := ServerTLSConfig(filepath.Join(dir, ServerCertFile), filepath.Join(dir, ServerKeyFile), filepath.Join(dir, "missing.crt"))
	if err != nil {
		t.Fatalf("ServerTLSConfig with missing ca must tolerate: %v", err)
	}
	if n := len(tlsCfg3.Certificates[0].Certificate); n != 1 {
		t.Fatalf("served chain len = %d, want 1 when CA unavailable", n)
	}
}

// TestEnsureRejectsIncompleteCA ca.crt 与 ca.key 任一单独存在（R10-#2）：
// 拒绝生成且不覆盖既有文件，报错须提示人工处置。
func TestEnsureRejectsIncompleteCA(t *testing.T) {
	// 场景一：ca.crt 在、ca.key 缺（旧实现会重新生成覆盖——正是审查指出的缺陷）。
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CACertFile), []byte("stub cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "不完整") || !strings.Contains(err.Error(), "人工") {
		t.Fatalf("cert-only CA must be rejected with human-action hint, got %v", err)
	}
	// 报错须点对缺失/存在的具体文件（ca.key 缺失、ca.crt 存在）。
	if !strings.Contains(err.Error(), "ca.key 缺失") || !strings.Contains(err.Error(), "ca.crt 存在") {
		t.Fatalf("error must name the right files, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, CACertFile)); string(b) != "stub cert" {
		t.Fatal("existing ca.crt must not be touched")
	}
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); !os.IsNotExist(err) {
		t.Fatal("no ca.key may be created when CA incomplete")
	}

	// 场景二：ca.key 在、ca.crt 缺（审查指出的覆盖路径本体：缺 ca.crt 时
	// 重新生成会覆盖既有 ca.key）。
	dir2 := filepath.Join(t.TempDir(), "pki")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, CAKeyFile), []byte("stub key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(Config{Dir: dir2}); err == nil || !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("key-only CA must be rejected, got %v", err)
	}
	// 报错须点对缺失/存在的具体文件（ca.crt 缺失、ca.key 存在）。
	if _, err := Ensure(Config{Dir: dir2}); err == nil ||
		!strings.Contains(err.Error(), "ca.crt 缺失") || !strings.Contains(err.Error(), "ca.key 存在") {
		t.Fatalf("error must name the right files, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir2, CAKeyFile)); string(b) != "stub key" {
		t.Fatal("existing ca.key must not be overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir2, CACertFile)); !os.IsNotExist(err) {
		t.Fatal("no ca.crt may be created when CA incomplete")
	}
}

// TestEnsureRequiresKeyPerms 复用既有私钥时权限必须 0600（R10-#2，非 Windows）。
func TestEnsureRequiresKeyPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows file mode semantics differ")
	}
	dir := filepath.Join(t.TempDir(), "pki")
	ensure(t, Config{Dir: dir})

	// ca.key 放宽为 0644 → 拒绝。
	if err := os.Chmod(filepath.Join(dir, CAKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("loose ca.key perms must be rejected, got %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, CAKeyFile), 0o600); err != nil {
		t.Fatal(err)
	}

	// server.key 放宽为 0644 → 拒绝。
	if err := os.Chmod(filepath.Join(dir, ServerKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(Config{Dir: dir}); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("loose server.key perms must be rejected, got %v", err)
	}

	// 恢复 0600 → 幂等通过。
	if err := os.Chmod(filepath.Join(dir, ServerKeyFile), 0o600); err != nil {
		t.Fatal(err)
	}
	res := ensure(t, Config{Dir: dir})
	if res.CAGenerated || res.ServerCertRenewed {
		t.Fatalf("idempotent re-run must be a no-op: %+v", res)
	}
}

// TestEnsureIdempotent 二次运行零改动：四文件字节级不变（含私钥不覆盖，SPEC 验收 1）。
func TestEnsureIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})
	before := readAll(t, dir)

	res := ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})
	if res.CAGenerated || res.ServerCertRenewed {
		t.Fatalf("second ensure must be a no-op: %+v", res)
	}
	after := readAll(t, dir)
	for f, b := range before {
		if string(after[f]) != string(b) {
			t.Fatalf("%s changed on second run", f)
		}
	}
}

// TestEnsureResignOnSANChange tailnet_ip 变更 → 用既有 CA + 既有私钥重签证书，
// CA 与两把私钥保持不动。
func TestEnsureResignOnSANChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})
	before := readAll(t, dir)
	oldFP := parseCert(t, before[ServerCertFile])

	res := ensure(t, Config{Dir: dir, TailnetIP: "100.64.1.77"})
	if res.CAGenerated {
		t.Fatal("CA must not be regenerated on SAN change")
	}
	if !res.ServerCertRenewed {
		t.Fatal("server cert should be renewed on SAN change")
	}
	after := readAll(t, dir)
	if string(after[CAKeyFile]) != string(before[CAKeyFile]) ||
		string(after[ServerKeyFile]) != string(before[ServerKeyFile]) {
		t.Fatal("private keys must never be overwritten")
	}
	if string(after[CACertFile]) != string(before[CACertFile]) {
		t.Fatal("ca cert must not change")
	}
	newCert := parseCert(t, after[ServerCertFile])
	if newCert.Equal(oldFP) {
		t.Fatal("server cert should differ after SAN change")
	}
	found := false
	for _, ip := range newCert.IPAddresses {
		if ip.String() == "100.64.1.77" {
			found = true
		}
	}
	if !found {
		t.Fatalf("new tailnet IP missing from SAN: %v", newCert.IPAddresses)
	}
}

// TestEnsureRejectsCorruptCA 已存在但损坏的 CA 必须报错，绝不静默覆盖
// （覆盖 CA = 作废全部已固定指纹的客户端）。
func TestEnsureRejectsCorruptCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CACertFile), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(Config{Dir: dir}); err == nil {
		t.Fatal("corrupt CA must be rejected")
	}
}

// TestEnsureRejectsExpiredCA 复用 CA 前校验有效期（R13-#2）：已过期/尚未生效
// 的 CA 不得用于重签（产物无法通过客户端验证），报错须提示重新 make pki，
// 且不得改动既有 CA 文件；对照：有效期内 CA 幂等复用不受影响。
func TestEnsureRejectsExpiredCA(t *testing.T) {
	for _, tc := range []struct {
		name        string
		notBefore   time.Duration // 相对当前时刻
		notAfter    time.Duration
		wantKeyword string
	}{
		{"expired", -25 * time.Hour, -time.Hour, "已过期"},
		{"not-yet-valid", time.Hour, 25 * time.Hour, "尚未生效"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := freshDirWithCAValidity(t, tc.notBefore, tc.notAfter)
			before := map[string][]byte{}
			for _, f := range []string{CACertFile, CAKeyFile} {
				b, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatal(err)
				}
				before[f] = b
			}
			_, err := Ensure(Config{Dir: dir})
			if err == nil || !strings.Contains(err.Error(), tc.wantKeyword) || !strings.Contains(err.Error(), "make pki") {
				t.Fatalf("%s CA must be rejected with make-pki hint, got %v", tc.name, err)
			}
			for _, f := range []string{CACertFile, CAKeyFile} {
				b, rerr := os.ReadFile(filepath.Join(dir, f))
				if rerr != nil || string(b) != string(before[f]) {
					t.Fatalf("existing %s must not be touched on rejection", f)
				}
			}
			// 拒绝时不得悄悄补齐/新生成服务端材料。
			if _, err := os.Stat(filepath.Join(dir, ServerCertFile)); !os.IsNotExist(err) {
				t.Fatal("no server cert may be issued from an unusable CA")
			}
		})
	}

	// 对照：新生成（有效期内）的 CA 再次 Ensure 正常复用（幂等零改动）。
	dir := filepath.Join(t.TempDir(), "pki")
	ensure(t, Config{Dir: dir})
	res := ensure(t, Config{Dir: dir})
	if res.CAGenerated {
		t.Fatalf("valid CA must be reused, got %+v", res)
	}
}

// freshDirWithCAValidity 生成配对 CA（ca.crt+ca.key）目录，有效期相对当前
// 时刻偏移指定，用于过期/未生效场景。
func freshDirWithCAValidity(t *testing.T, notBefore, notAfter time.Duration) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Validity Test CA"},
		NotBefore:             time.Now().Add(notBefore),
		NotAfter:              time.Now().Add(notAfter),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CACertFile),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CAKeyFile),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rewriteServerCert 用目录内既有 CA + 服务端私钥重签一张按 mutate 改写的服务端
// 证书写回 server.crt（模拟历史遗留的不可用证书；SAN 与 issueServerCert 同源）。
func rewriteServerCert(t *testing.T, dir string, mutate func(tpl *x509.Certificate)) {
	t.Helper()
	caCert := parseCert(t, readAll(t, dir)[CACertFile])
	caKey, err := loadKey(filepath.Join(dir, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := loadKey(filepath.Join(dir, ServerKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	dnsNames, ipAddrs := sans(Config{TailnetIP: "100.64.0.9"})
	tpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "meshconsole", Organization: []string{"meshconsole"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}
	if mutate != nil {
		mutate(tpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ServerCertFile),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureRenewsUnusableServerCert 既有服务端证书「尚未生效」或「缺 ServerAuth
// EKU」时走既有重签路径（R15-#2：CA 与两把私钥不动）；重签产物当期有效且带
// ServerAuth。对照：EKU 仅含 ExtKeyUsageAny 视为可用于服务端，保持不重签。
func TestEnsureRenewsUnusableServerCert(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(tpl *x509.Certificate)
	}{
		{"not-yet-valid", func(tpl *x509.Certificate) { tpl.NotBefore = time.Now().Add(time.Hour) }},
		{"missing-server-auth-eku", func(tpl *x509.Certificate) { tpl.ExtKeyUsage = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "pki")
			ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})
			rewriteServerCert(t, dir, tc.mutate)
			before := readAll(t, dir)

			res := ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})
			if res.CAGenerated || !res.ServerCertRenewed {
				t.Fatalf("unusable server cert (%s) must trigger renewal, got %+v", tc.name, res)
			}
			after := readAll(t, dir)
			for _, f := range []string{CAKeyFile, ServerKeyFile, CACertFile} {
				if string(after[f]) != string(before[f]) {
					t.Fatalf("%s must not change on renewal", f)
				}
			}
			newCert := parseCert(t, after[ServerCertFile])
			now := time.Now()
			if now.Before(newCert.NotBefore) || now.After(newCert.NotAfter) {
				t.Fatalf("renewed cert must be valid now, NotBefore=%s NotAfter=%s", newCert.NotBefore, newCert.NotAfter)
			}
			hasServerAuth := false
			for _, eku := range newCert.ExtKeyUsage {
				if eku == x509.ExtKeyUsageServerAuth {
					hasServerAuth = true
				}
			}
			if !hasServerAuth {
				t.Fatalf("renewed cert must carry ServerAuth EKU, got %v", newCert.ExtKeyUsage)
			}
		})
	}

	// 对照：仅含 ExtKeyUsageAny → 保持既有证书（幂等零改动，不重签）。
	dir := filepath.Join(t.TempDir(), "pki")
	ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})
	rewriteServerCert(t, dir, func(tpl *x509.Certificate) {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	})
	res := ensure(t, Config{Dir: dir, TailnetIP: "100.64.0.9"})
	if res.CAGenerated || res.ServerCertRenewed {
		t.Fatalf("ExtKeyUsageAny must count as server auth and keep the cert, got %+v", res)
	}
}

// TestServerTLSConfigRequiresKeyPerms 启动加载入口校验私钥 0600（R15-#3，
// 与 R10-#2 生成侧同口径，非 Windows）：放宽 0644 拒绝并提示 chmod，0600 正常通过。
func TestServerTLSConfigRequiresKeyPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows file mode semantics differ")
	}
	dir := filepath.Join(t.TempDir(), "pki")
	ensure(t, Config{Dir: dir})
	certPath := filepath.Join(dir, ServerCertFile)
	keyPath := filepath.Join(dir, ServerKeyFile)

	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ServerTLSConfig(certPath, keyPath, "")
	if err == nil || !strings.Contains(err.Error(), "0600") || !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("loose key perms must be rejected with chmod hint, got %v", err)
	}

	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerTLSConfig(certPath, keyPath, ""); err != nil {
		t.Fatalf("0600 key must load, got %v", err)
	}
}

func parseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		t.Fatal("no PEM block")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// TestEnsureRejectsMismatchedCAPair 复用 CA 时校验证书/私钥配对（R11-D）：
// ca.crt 与 ca.key 来自不同密钥对 → 拒绝（否则重签的证书无法被 ca.crt 验证
// 却报成功）。
func TestEnsureRejectsMismatchedCAPair(t *testing.T) {
	if _, err := Ensure(Config{Dir: freshDirWithMismatchedCA(t)}); err == nil || !strings.Contains(err.Error(), "不配对") {
		t.Fatalf("mismatched CA pair must be rejected, got %v", err)
	}
}

// freshDirWithMismatchedCA 生成「CA A 的证书 + CA B 的私钥」目录。
func freshDirWithMismatchedCA(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyA, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "CA A"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &keyA.PublicKey, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CACertFile),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(keyB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CAKeyFile),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEnsureRejectsInvalidTailnetIP R17-#1：tailnet_ip 配置了但非法 → 拒绝生成
// （旧实现静默忽略该 SAN 仍报成功）。合法值不受影响（幂等用例另行覆盖）。
func TestEnsureRejectsInvalidTailnetIP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	_, err := Ensure(Config{Dir: dir, TailnetIP: "not-an-ip"})
	if err == nil || !strings.Contains(err.Error(), "tailnet_ip") {
		t.Fatalf("err = %v, want tailnet_ip validation error", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("no artifacts should be created on invalid input, got %d files", len(entries))
	}
	// 对照：合法 tailnet_ip 正常生成且 SAN 含该地址（既有用例也覆盖，这里快速复核）。
	res, err := Ensure(Config{Dir: filepath.Join(t.TempDir(), "pki2"), TailnetIP: "100.64.0.9"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.ServerCertPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ip := range cert.IPAddresses {
		if ip.String() == "100.64.0.9" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tailnet IP missing from SAN: %v", cert.IPAddresses)
	}
}
