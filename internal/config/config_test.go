package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConsoleDefaultsAndOverride(t *testing.T) {
	// 无文件 → 纯缺省。
	cfg, err := LoadConsole("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:7700" || cfg.OfflineAfterS != 60 || cfg.MetricsRetentionDays != 7 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}

	path := filepath.Join(t.TempDir(), "console.yaml")
	yaml := `
listen: "0.0.0.0:7800"
db_path: "/tmp/x.db"
registration_tokens:
  - token: "tok-0123456789abcdef"
    expires_at: "2099-01-01T00:00:00Z"
    expected_node: "n1"
offline_after_s: 90
metrics_retention_days: 3
log_level: "debug"
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConsole(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:7800" || cfg.OfflineAfterS != 90 || cfg.LogLevel != "debug" {
		t.Fatalf("override wrong: %+v", cfg)
	}
}

func TestConsoleValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.yaml")
	// 缺 registration_tokens 必须拒绝启动（认证从第一行代码就在）。
	if err := os.WriteFile(path, []byte("listen: \":7700\"\ndb_path: \"x.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConsole(path); err == nil || !strings.Contains(err.Error(), "registration_tokens") {
		t.Fatalf("want registration_tokens error, got %v", err)
	}
	// 短 token 拒绝（映射写法下仍校验长度）。
	if err := os.WriteFile(path, []byte(`registration_tokens:
  - token: "short"
    expires_at: "2099-01-01T00:00:00Z"
    expected_node: "n1"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConsole(path); err == nil || !strings.Contains(err.Error(), "16") {
		t.Fatalf("short token should be rejected, got %v", err)
	}
}

// TestConsolePlainStringTokenRejected 纯字符串写法在解析层即拒绝并提示新写法
// （R3-#4 强制化：无到期、不绑节点的 token 等于永久凭据后门）；报错不回显原值
// （R15-#1：凭据材料不进错误信息/日志，R11-C/R13-#3 同口径）。
func TestConsolePlainStringTokenRejected(t *testing.T) {
	const secret = "plain-token-0123456789ab"
	path := filepath.Join(t.TempDir(), "console.yaml")
	yaml := "registration_tokens:\n  - \"" + secret + "\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConsole(path)
	if err == nil {
		t.Fatal("plain string token must be rejected")
	}
	for _, want := range []string{"纯字符串", "映射", "expires_at", "expected_node"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should hint new form (missing %q)", err, want)
		}
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must not echo the plain token value, got %q", err)
	}
}

// TestConsoleTokenFieldsRequired 映射写法缺 expires_at / expected_node 一律拒绝加载（R3-#4）。
func TestConsoleTokenFieldsRequired(t *testing.T) {
	cases := map[string]string{
		"missing expires_at": `registration_tokens:
  - token: "tok-noexpiry-012345678"
    expected_node: "n1"
`,
		"blank expires_at": `registration_tokens:
  - token: "tok-blankexp-01234567"
    expires_at: ""
    expected_node: "n1"
`,
		"missing expected_node": `registration_tokens:
  - token: "tok-nobinding-0123456"
    expires_at: "2099-01-01T00:00:00Z"
`,
	}
	for name, yaml := range cases {
		path := filepath.Join(t.TempDir(), "console.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConsole(path); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
}

// TestConsoleStructuredTokens 结构化注册 token（到期 + 预期节点绑定，R1-#4 强制化）：
// 映射写法解析正常，带引号与裸时间戳均可；空 token 字段拒绝。
func TestConsoleStructuredTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.yaml")
	yaml := `
registration_tokens:
  - token: "tok-bound-0123456789ab"
    expires_at: "2099-01-01T00:00:00Z"
    expected_node: "mac-mini"
  - token: "tok-exp-0123456789abcd"
    expires_at: 2099-01-01T00:00:00Z
    expected_node: "node-b"
  - token: "tok-pad-0123456789012"
    expires_at: 2099-01-01T00:00:00Z
    expected_node: "  padded  "
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConsole(path)
	if err != nil {
		t.Fatal(err)
	}
	toks := cfg.RegistrationTokens
	if len(toks) != 3 {
		t.Fatalf("tokens = %d, want 3", len(toks))
	}
	if toks[0].ExpectedNode != "mac-mini" {
		t.Fatalf("expected_node = %q, want mac-mini", toks[0].ExpectedNode)
	}
	want := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	if !toks[0].ExpiresAt.Time.Equal(want) {
		t.Fatalf("quoted expires_at = %v, want %v", toks[0].ExpiresAt.Time, want)
	}
	if !toks[1].ExpiresAt.Time.Equal(want) {
		t.Fatalf("bare expires_at = %v, want %v", toks[1].ExpiresAt.Time, want)
	}
	// expected_node 加载时去首尾空白，保证与注册请求里的名字可比。
	if toks[2].ExpectedNode != "padded" {
		t.Fatalf("expected_node = %q, want padded", toks[2].ExpectedNode)
	}
	// 空 token 字段在解析层即拒绝。
	path2 := filepath.Join(t.TempDir(), "console.yaml")
	if err := os.WriteFile(path2, []byte("registration_tokens:\n  - expires_at: \"2099-01-01T00:00:00Z\"\n    expected_node: n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConsole(path2); err == nil {
		t.Fatal("token entry without token field must be rejected")
	}
}

// TestConsoleExpiredTokenRejected 启动时已过期的 token 拒绝加载（审查 R1-#4）。
func TestConsoleExpiredTokenRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.yaml")
	yaml := `
registration_tokens:
  - token: "expired-0123456789abcd"
    expires_at: "2020-01-01T00:00:00Z"
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConsole(path)
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("expired token should be rejected, got %v", err)
	}
}

func TestAgentDefaults(t *testing.T) {
	cfg, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CollectIntervalS != 15 || cfg.Role != "node" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if DefaultDiskMount() != "/" && !strings.Contains(DefaultDiskMount(), ":") {
		t.Fatalf("disk mount default odd: %q", DefaultDiskMount())
	}
}

// TestExpandHome ~ 展开（审查 R1-#11）。
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~":                   home,
		"~/.meshagent/s.json": filepath.Join(home, ".meshagent", "s.json"),
		"~root/x":             "~root/x", // ~user 不支持，原样保留
		"/abs/path":           "/abs/path",
		"":                    "",
		// "~\\win\\path" 的展开结果取决于平台 filepath 语义（Windows 下 Join 才认反斜杠），不做跨平台断言。
	}
	for in, want := range cases {
		if got := ExpandHome(in); got != want {
			t.Fatalf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAgentStateFileTilde 配置里的 ~ 必须展开为真实 home（与默认 state 路径一致）。
func TestAgentStateFileTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("state_file: \"~/.meshagent/state.json\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != DefaultStatePath() {
		t.Fatalf("state_file = %q, want default %q", cfg.StateFile, DefaultStatePath())
	}
}

// TestAgentServiceValidation 受管服务声明校验（SPEC-M1b-a §4：type 非法值拒绝启动）。
func TestAgentServiceValidation(t *testing.T) {
	cases := map[string]string{
		"bad type": `services:
  - name: s1
    type: kubernetes
    target: s1
`,
		"bad charset": `services:
  - name: s1
    type: systemd
    target: "foo;rm -rf /"
`,
		"empty target": `services:
  - name: s1
    type: docker
    target: ""
`,
		"duplicate name": `services:
  - name: s1
    type: process
    target: a
  - name: s1
    type: process
    target: b
`,
		"process leading dash": `services:
  - name: s1
    type: process
    target: "-evil"
`,
		"empty name": `services:
  - name: ""
    type: process
    target: a
`,
	}
	for name, yaml := range cases {
		path := filepath.Join(t.TempDir(), "agent.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAgent(path); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
	// 合法声明：systemd/docker/process 均接受，target 字符集校验通过。
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(`services:
  - name: rustdesk
    type: systemd
    target: rustdesk@user.service
  - name: derp
    type: docker
    target: derp-1
  - name: helper
    type: process
    target: Visual Studio Code
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("valid services rejected: %v", err)
	}
	if len(cfg.Services) != 3 || cfg.Services[0].Target != "rustdesk@user.service" {
		t.Fatalf("services parsed wrong: %+v", cfg.Services)
	}
}

// TestValidServiceTarget systemd/docker target 白名单字符集逐字符验证。
func TestValidServiceTarget(t *testing.T) {
	valid := []string{"rustdesk", "nginx.service", "user@1000", "my-app_2", "a.b-c_d@e"}
	invalid := []string{"", "a b", "a;b", "a/b", "a$b", "a\nb", "中文", "a|b", "-x", "`x`", "x'y"}
	for _, s := range valid {
		if !ValidServiceTarget(s) {
			t.Fatalf("%q should be valid", s)
		}
	}
	for _, s := range invalid {
		if ValidServiceTarget(s) {
			t.Fatalf("%q should be invalid", s)
		}
	}
}

// TestAgentScanValidation 自定义 agent 声明校验（SPEC-M1b-a §4：相对路径拒绝启动）。
func TestAgentScanValidation(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"relative command": `agent_scan:
  custom:
    - name: my-agent
      type: cli
      command: bin/my-agent
`,
		"bad custom type": `agent_scan:
  custom:
    - name: my-agent
      type: service
      command: /bin/true
`,
		"port out of range": `agent_scan:
  services:
    - name: hana-agent
      port: 70000
`,
		"dup across custom/services": `agent_scan:
  custom:
    - name: x
      command: /bin/true
  services:
    - name: x
      port: 5800
`,
		"dup known": `agent_scan:
  known: ["zcode", "zcode"]
`,
	}
	for name, yaml := range cases {
		path := filepath.Join(t.TempDir(), "agent.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAgent(path); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}

	// 合法声明 + 缺省值填充：version_flag 缺省 --version，~/command 展开，known 缺省取内置清单。
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(`agent_scan:
  custom:
    - name: my-agent
      command: ~/tools/my-agent
  services:
    - name: hana-agent
      port: 5800
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("valid scan config rejected: %v", err)
	}
	scan := cfg.ScanCfg()
	if scan.Custom[0].VersionFlag != "--version" || scan.Custom[0].Type != "cli" {
		t.Fatalf("custom defaults wrong: %+v", scan.Custom[0])
	}
	if scan.Custom[0].Command != filepath.Join(home, "tools", "my-agent") {
		t.Fatalf("command not expanded: %q", scan.Custom[0].Command)
	}
	if scan.Known != nil {
		t.Fatal("known should be nil when not configured")
	}
	if got := scan.KnownList(); len(got) != 5 {
		t.Fatalf("default known list = %v, want 5 entries", got)
	}

	// known 显式给出（含空列表）→ 整体替换。
	path2 := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path2, []byte("agent_scan:\n  known: [\"custom-cli\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadAgent(path2)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg2.ScanCfg().KnownList(); len(got) != 1 || got[0] != "custom-cli" {
		t.Fatalf("known override = %v", got)
	}
}

// TestAgentTLSPinningRequired https 上报必须配置 ca_cert 或 fingerprint 之一（SPEC-M1b-a §1）。
func TestAgentTLSPinningRequired(t *testing.T) {
	// https + 两者皆空 → 拒绝启动。
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(`console_url: "https://127.0.0.1:7700"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(path); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("https without pinning must be rejected, got %v", err)
	}

	// 带 fingerprint（冒号分隔大写也要接受并归一化）。
	path2 := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path2, []byte(`console_url: "https://127.0.0.1:7700"
fingerprint: "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgent(path2)
	if err != nil {
		t.Fatalf("https with fingerprint rejected: %v", err)
	}
	if cfg.Fingerprint != "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Fatalf("fingerprint not normalized: %q", cfg.Fingerprint)
	}

	// 坏 fingerprint（长度不足/非 hex）拒绝。
	for _, bad := range []string{"abcd", "zzzz", "AB:CD"} {
		path3 := filepath.Join(t.TempDir(), "agent.yaml")
		if err := os.WriteFile(path3, []byte("fingerprint: \""+bad+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAgent(path3); err == nil {
			t.Fatalf("bad fingerprint %q must be rejected", bad)
		}
	}

	// http 上报不再有任何豁免（R10-#1）：明文 console_url 拒绝加载。
	path4 := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path4, []byte("console_url: \"http://127.0.0.1:7700\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err4 := LoadAgent(path4)
	if err4 == nil || !strings.Contains(err4.Error(), "https") {
		t.Fatalf("plain http console_url must be rejected, got %v", err4)
	}
}

// TestAgentDockerBin docker CLI 路径配置（R10-#4）：缺省 "docker"，可覆盖，空串归缺省。
func TestAgentDockerBin(t *testing.T) {
	cfg, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DockerBin != "docker" {
		t.Fatalf("default docker_bin = %q, want docker", cfg.DockerBin)
	}
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("docker_bin: \"/usr/local/bin/docker-wrapper\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DockerBin != "/usr/local/bin/docker-wrapper" {
		t.Fatalf("docker_bin override = %q", cfg.DockerBin)
	}
	if err := os.WriteFile(path, []byte("docker_bin: \"   \"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadAgent(path); err != nil || cfg.DockerBin != "docker" {
		t.Fatalf("blank docker_bin should fall back to default, got %q err=%v", cfg.DockerBin, err)
	}
}

// TestAgentScanTotalCap 合并去重后的 agent 总数上限 64（R10-#7）：65 个不同名
// 条目拒绝启动；重名去重后 ≤64 则通过（同名声明覆盖 PATH 探测，不重复计数）。
func TestAgentScanTotalCap(t *testing.T) {
	names := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = "cli-" + strings.Repeat("a", 1) + strconv.Itoa(i)
		}
		return `["` + strings.Join(parts, `","`) + `"]`
	}
	// 65 个不同名（known 60 + services 5）→ 拒绝。
	path := filepath.Join(t.TempDir(), "agent.yaml")
	yaml := "agent_scan:\n  known: " + names(60) + "\n  services:\n"
	for i := 0; i < 5; i++ {
		yaml += "    - name: svc-" + strconv.Itoa(i) + "\n      port: " + strconv.Itoa(5800+i) + "\n"
	}
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAgent(path)
	if err == nil || !strings.Contains(err.Error(), "64") {
		t.Fatalf("65 merged agents must be rejected, got %v", err)
	}
	// 60 known + 1 个与 known 同名的 service → 合并去重 60，通过。
	path2 := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path2, []byte("agent_scan:\n  known: "+names(60)+"\n  services:\n    - name: cli-0\n      port: 5800\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(path2); err != nil {
		t.Fatalf("merged-unique ≤64 must be accepted (dedupe by name), got %v", err)
	}
}

// TestConsolePKIFields console 侧 TLS/限额字段缺省与覆盖（SPEC-M1b-a §1）。
func TestConsolePKIFields(t *testing.T) {
	cfg, err := LoadConsole("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PKIDir != "./pki" || cfg.MaxConnections != 256 {
		t.Fatalf("pki defaults wrong: pki_dir=%q max_conn=%d", cfg.PKIDir, cfg.MaxConnections)
	}
	if cert, key := cfg.CertKeyPaths(); cert != "pki/server.crt" || key != "pki/server.key" {
		t.Fatalf("CertKeyPaths defaults = %q/%q", cert, key)
	}

	path := filepath.Join(t.TempDir(), "console.yaml")
	if err := os.WriteFile(path, []byte("pki_dir: /opt/meshconsole/pki\ntls_cert: /x/server.pem\ntailnet_ip: \"100.64.0.1\"\nmax_connections: 32\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConsoleForPKI(path) // 无 registration_tokens 也应可加载（pki 子命令路径）
	if err != nil {
		t.Fatalf("LoadConsoleForPKI without tokens must work: %v", err)
	}
	if cfg.PKIDir != "/opt/meshconsole/pki" || cfg.MaxConnections != 32 || cfg.TailnetIP != "100.64.0.1" {
		t.Fatalf("pki override wrong: %+v", cfg)
	}
	if cert, key := cfg.CertKeyPaths(); cert != "/x/server.pem" || key != "/opt/meshconsole/pki/server.key" {
		t.Fatalf("CertKeyPaths override = %q/%q", cert, key)
	}

	// max_connections 非正数拒绝。
	path2 := filepath.Join(t.TempDir(), "console.yaml")
	if err := os.WriteFile(path2, []byte("max_connections: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConsole(path2); err == nil {
		t.Fatal("max_connections=0 must be rejected")
	}
}

// TestServiceTargetLengthCap R17-#2：systemd/docker target 超过 256 字节 → 启动
// 拒绝（与服务端入库截断口径一致，防运行期静默截断导致状态错位）。
func TestServiceTargetLengthCap(t *testing.T) {
	long := strings.Repeat("a", 257)
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	body := "services:\n  - name: svc\n    type: systemd\n    target: " + long + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(p); err == nil || !strings.Contains(err.Error(), "256") {
		t.Fatalf("err = %v, want target length cap rejection", err)
	}
	// 256 字节整边界合法。
	ok := "services:\n  - name: svc\n    type: systemd\n    target: " + strings.Repeat("a", 256) + "\n"
	if err := os.WriteFile(p, []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(p); err != nil {
		t.Fatalf("256-byte target should pass: %v", err)
	}
}

// TestHeadscaleURLScheme R19-#10：headscale url 的 scheme 限 http/https——
// 其他 scheme（ftp/file/ssh/javascript 等）与无 scheme 形式启动即拒绝；
// http/https 正常加载。
func TestHeadscaleURLScheme(t *testing.T) {
	dir := t.TempDir()
	write := func(u string) string {
		path := filepath.Join(dir, "console.yaml")
		body := "headscale:\n  url: \"" + u + "\"\n  api_key: \"k\"\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, bad := range []string{"ftp://127.0.0.1:8080", "file:///tmp/hs", "ssh://127.0.0.1", "127.0.0.1:8080", "javascript:alert(1)"} {
		if _, err := LoadConsoleForPKI(write(bad)); err == nil || !strings.Contains(err.Error(), "HTTP(S)") {
			t.Fatalf("headscale url %q: err = %v, want HTTP(S) scheme rejection", bad, err)
		}
	}
	for _, good := range []string{"http://127.0.0.1:8080", "https://headscale.internal:8443"} {
		cfg, err := LoadConsoleForPKI(write(good))
		if err != nil {
			t.Fatalf("headscale url %q: %v", good, err)
		}
		if cfg.Headscale == nil || cfg.Headscale.URL != good {
			t.Fatalf("headscale url = %+v, want %q", cfg.Headscale, good)
		}
	}
}
