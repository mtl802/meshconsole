// config_m1bc2_test.go 为 M1b-c2 新配置项测试（SPEC §4/§5/§6）：
// panel_allowed_hosts / agent_allowed_cidrs / tls_extra_sans 的缺省、显式空、
// 非法项拒绝、规范化去重，以及公网门槛的 config 文件安全检查。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg 写临时配置文件并载入（LoadConsole）。
func loadFromYAML(t *testing.T, body string) (*Console, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "console.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConsole(path)
}

func validBase() string {
	return `
listen: "0.0.0.0:7700"
db_path: "./data/meshconsole.db"
registration_tokens:
  - token: "abcdefghijklmnopqrstuvwxyz012345"
    expires_at: "2036-12-31T23:59:59Z"
    expected_node: "node-1"
`
}

// TestPanelAllowedHostsDefaults 缺省（键缺席）= 回环名单；显式空数组 = 仅回环
// （同值语义）；自定义条目规范化（大小写、IPv6 方括号、去重保序）。
func TestPanelAllowedHostsDefaults(t *testing.T) {
	// 键缺席。
	cfg, err := loadFromYAML(t, validBase())
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.AllowedHosts()
	if len(got) != 3 || got[0] != "localhost" || got[1] != "127.0.0.1" || got[2] != "::1" {
		t.Fatalf("default hosts = %v", got)
	}
	// 显式空数组 → 仅回环。
	cfg, err = loadFromYAML(t, validBase()+"panel_allowed_hosts: []\n")
	if err != nil {
		t.Fatal(err)
	}
	if got = cfg.AllowedHosts(); len(got) != 3 {
		t.Fatalf("explicit empty = %v, want loopback list", got)
	}
	// 自定义名单：规范化 + 去重。
	cfg, err = loadFromYAML(t, validBase()+`panel_allowed_hosts: ["100.64.0.3", "1.13.158.180", "[2001:DB8::1]", "Panel.Example.COM", "100.64.0.3"]`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	got = cfg.AllowedHosts()
	want := []string{"100.64.0.3", "1.13.158.180", "2001:db8::1", "panel.example.com"}
	if len(got) != len(want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hosts[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPanelAllowedHostsInvalid 非法条目启动即报错：空值、scheme、端口、path。
func TestPanelAllowedHostsInvalid(t *testing.T) {
	for _, bad := range []string{
		`panel_allowed_hosts: [""]`,
		`panel_allowed_hosts: ["http://example.com"]`,
		`panel_allowed_hosts: ["example.com:8443"]`,
		`panel_allowed_hosts: ["example.com/x"]`,
		`panel_allowed_hosts: ["evil example.com"]`,
		`panel_allowed_hosts: ["not a host!"]`,
	} {
		if _, err := loadFromYAML(t, validBase()+bad+"\n"); err == nil {
			t.Fatalf("%q must fail to load", bad)
		}
	}
}

// TestAgentAllowedCIDRs 缺省 = 私网清单（含 100.64/10）；显式空 = 仅回环；
// 非法条目报错。
func TestAgentAllowedCIDRs(t *testing.T) {
	cfg, err := loadFromYAML(t, validBase())
	if err != nil {
		t.Fatal(err)
	}
	def := cfg.AgentCIDRs()
	if len(def) != 6 {
		t.Fatalf("default cidrs = %d, want 6", len(def))
	}
	cfg, err = loadFromYAML(t, validBase()+"agent_allowed_cidrs: []\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AgentCIDRs(); len(got) != 2 {
		t.Fatalf("explicit empty = %d, want 2 (loopback v4+v6)", len(got))
	}
	cfg, err = loadFromYAML(t, validBase()+`agent_allowed_cidrs: ["100.64.0.0/10"]`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AgentCIDRs(); len(got) != 1 || got[0].String() != "100.64.0.0/10" {
		t.Fatalf("custom = %v", got)
	}
	if _, err := loadFromYAML(t, validBase()+`agent_allowed_cidrs: ["1.2.3.4"]`+"\n"); err == nil {
		t.Fatal("non-cidr entry must fail")
	}
}

// TestTLSExtraSANsPassthrough 配置直通（语义校验在 pki.NormalizeSANs，pki 侧测）。
func TestTLSExtraSANsPassthrough(t *testing.T) {
	cfg, err := loadFromYAML(t, validBase()+`tls_extra_sans: ["1.13.158.180"]`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TLSExtraSANs) != 1 || cfg.TLSExtraSANs[0] != "1.13.158.180" {
		t.Fatalf("extra sans = %v", cfg.TLSExtraSANs)
	}
}

// TestCheckConfigFileSecurity 公网门槛②：普通文件 + 0600 + 属运行用户。
func TestCheckConfigFileSecurity(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.yaml")
	if err := os.WriteFile(ok, []byte("listen: 127.0.0.1:7700\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigFileSecurity(ok); err != nil {
		t.Fatalf("0600 regular file: %v", err)
	}
	loose := filepath.Join(dir, "loose.yaml")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigFileSecurity(loose); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("0644 file: %v", err)
	}
	if err := CheckConfigFileSecurity(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}
	if err := CheckConfigFileSecurity(dir); err == nil {
		t.Fatal("directory must fail (not a regular file)")
	}
}
