package config

import (
	"os"
	"path/filepath"
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
// （R3-#4 强制化：无到期、不绑节点的 token 等于永久凭据后门）。
func TestConsolePlainStringTokenRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.yaml")
	yaml := "registration_tokens:\n  - \"plain-token-0123456789ab\"\n"
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
