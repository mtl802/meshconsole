// pki_m1bc2_test.go 为 SAN 统一校验测试（SPEC-M1b-c2 §6）：IP/DNS 分类、
// URL/端口/CIDR/空值显式拒绝、规范化去重、以及 extra sans 触发证书重签且
// SAN 落证。
package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestNormalizeSANs 分类与拒绝矩阵（SPEC §6 全口径）。
func TestNormalizeSANs(t *testing.T) {
	// 合法：IP 字面量（v4/v6）+ DNS（大小写/尾点规范化）+ 去重保序。
	got, err := NormalizeSANs([]string{
		"1.13.158.180", "Panel.Example.COM.", "2001:db8::1",
		"1.13.158.180", "panel.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("entries = %+v, want 3 (deduped)", got)
	}
	if got[0].IP == nil || got[0].IP.String() != "1.13.158.180" {
		t.Fatalf("entry0 = %+v", got[0])
	}
	if got[1].DNS != "panel.example.com" || got[1].IP != nil {
		t.Fatalf("entry1 = %+v", got[1])
	}
	if got[2].IP == nil || !got[2].IP.Equal(net.ParseIP("2001:db8::1")) {
		t.Fatalf("entry2 = %+v", got[2])
	}
	// 非法：URL / 端口 / CIDR / 空值 / 非 DNS 字符。
	for _, bad := range []string{
		"https://example.com",
		"example.com:8443",
		"1.13.158.180/32",
		"",
		"   ",
		"not_a_host",
		"-lead.example.com",
		"ex..ample.com",
	} {
		if _, err := NormalizeSANs([]string{bad}); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

// TestEnsureExtraSANsIssueAndRenew 追加 SAN 触发重签：首次生成含 extra IP SAN；
// 再次运行幂等保持；extra 变更后 needsRenewal 判真重签并落新 SAN。
func TestEnsureExtraSANsIssueAndRenew(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	res, err := Ensure(Config{Dir: dir, TailnetIP: "100.64.0.3"})
	if err != nil || !res.ServerCertRenewed {
		t.Fatalf("first ensure: %+v, %v", res, err)
	}
	// 无 extra：幂等。
	res, err = Ensure(Config{Dir: dir, TailnetIP: "100.64.0.3"})
	if err != nil || res.ServerCertRenewed {
		t.Fatalf("idempotent: %+v, %v", res, err)
	}
	// 加公网 IP SAN：重签且证书带该 SAN（修复旧 ExtraIPs 静默跳过）。
	res, err = Ensure(Config{Dir: dir, TailnetIP: "100.64.0.3", ExtraSANs: []string{"1.13.158.180"}})
	if err != nil || !res.ServerCertRenewed {
		t.Fatalf("renew with extra: %+v, %v", res, err)
	}
	cert := parseServerCert(t, filepath.Join(dir, ServerCertFile))
	found := false
	for _, ip := range cert.IPAddresses {
		if ip.String() == "1.13.158.180" {
			found = true
		}
	}
	if !found {
		t.Fatalf("extra IP SAN missing: %v", cert.IPAddresses)
	}
	// 同配置再次运行：保持不动。
	res, err = Ensure(Config{Dir: dir, TailnetIP: "100.64.0.3", ExtraSANs: []string{"1.13.158.180"}})
	if err != nil || res.ServerCertRenewed {
		t.Fatalf("idempotent with extra: %+v, %v", res, err)
	}
	// 非法 extra：任何写入前显式报错（全新目录也不留产物）。
	empty := filepath.Join(t.TempDir(), "fresh")
	if _, err := Ensure(Config{Dir: empty, ExtraSANs: []string{"https://bad"}}); err == nil {
		t.Fatal("invalid extra san must fail ensure")
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("failed validation must not create pki dir")
	}
}

func parseServerCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		t.Fatal("no pem block")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
