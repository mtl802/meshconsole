// main_m1bc2_test.go 为 M1b-c2 入口层测试（SPEC §1/§4）：
// 公网模式启动门槛（回环放行 / 公网逐项检查）、configFlag 提取、user/token
// CLI 全流程（注入口令读取，不走 TTY）。
package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

func testLoggerNop() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// gateEnv 准备一套门槛测试环境：库（可选预置启用用户）+ 配置文件（指定权限）。
func gateEnv(t *testing.T, withUser bool, cfgPerm os.FileMode) (net.Addr, *store.Store, *config.Console, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data", "meshconsole.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if withUser {
		if _, err := st.CreateUser(t.Context(), "lunge", "hash"); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "console.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: \"0.0.0.0:7700\"\n"), cfgPerm); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConsoleForPKI(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := net.ResolveTCPAddr("tcp", "0.0.0.0:7700")
	if err != nil {
		t.Fatal(err)
	}
	return pub, st, cfg, cfgPath
}

// TestPublicModeGate 门槛判定与逐项检查（SPEC §4）。
func TestPublicModeGate(t *testing.T) {
	loop, err := net.ResolveTCPAddr("tcp", "127.0.0.1:7700")
	if err != nil {
		t.Fatal(err)
	}
	// 回环：直接放行（无用户也放行——门槛只在公网形态生效）。
	if err := publicModeGate(loop, nil, "", nil, nil); err != nil {
		t.Fatalf("loopback gate = %v, want nil", err)
	}
	// 公网 + 无启用用户：拒绝并提示 user add。
	pub, st, cfg, cfgPath := gateEnv(t, false, 0o600)
	err = publicModeGate(pub, cfg, cfgPath, st, testLoggerNop())
	if err == nil || !strings.Contains(err.Error(), "user add") {
		t.Fatalf("no-user gate = %v, want user-add hint", err)
	}
	// 公网 + 启用用户 + 0600 config：放行。
	pub, st, cfg, cfgPath = gateEnv(t, true, 0o600)
	if err := publicModeGate(pub, cfg, cfgPath, st, testLoggerNop()); err != nil {
		t.Fatalf("passing gate = %v, want nil", err)
	}
	// 公网 + 启用用户 + 0644 config：拒绝（0600 要求）。
	loose := filepath.Join(t.TempDir(), "loose.yaml")
	if err := os.WriteFile(loose, []byte("listen: \"0.0.0.0:7700\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publicModeGate(pub, cfg, loose, st, testLoggerNop()); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("loose config gate = %v, want 0600 hint", err)
	}
	// config 缺失：拒绝（stat 失败）。
	if err := publicModeGate(pub, cfg, filepath.Join(t.TempDir(), "gone.yaml"), st, testLoggerNop()); err == nil {
		t.Fatal("missing config gate = nil, want error")
	}
}

// TestConfigFlag 提取 -config 两种写法与缺省。
func TestConfigFlag(t *testing.T) {
	if got := configFlag([]string{"add", "lunge"}); got != "console.yaml" {
		t.Fatalf("default = %q", got)
	}
	if got := configFlag([]string{"add", "lunge", "-config", "/tmp/a.yaml"}); got != "/tmp/a.yaml" {
		t.Fatalf("space form = %q", got)
	}
	if got := configFlag([]string{"-config=/tmp/b.yaml", "list"}); got != "/tmp/b.yaml" {
		t.Fatalf("= form = %q", got)
	}
}

// TestSecureHeaderWriter 全局安全头包装（R33-#4，SPEC §7「全链」口径）：
// 显式 WriteHeader（含 404/405 等错误码）与隐式 200（直接 Write）都在落笔前
// 携带 no-store/nosniff；mux 默认错误路径端到端覆盖。
func TestSecureHeaderWriter(t *testing.T) {
	check := func(rec *httptest.ResponseRecorder, code int, phase string) {
		t.Helper()
		if rec.Code != code {
			t.Fatalf("%s: code = %d, want %d", phase, rec.Code, code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: headers = %v, want no-store+nosniff", phase, rec.Header())
		}
	}
	// 显式错误状态码。
	rec := httptest.NewRecorder()
	(&secureHeaderWriter{ResponseWriter: rec}).WriteHeader(http.StatusForbidden)
	check(rec, http.StatusForbidden, "explicit 403")
	// 隐式 200（handler 直接 Write 不调 WriteHeader——agent API 的实际写法）。
	rec = httptest.NewRecorder()
	_, _ = (&secureHeaderWriter{ResponseWriter: rec}).Write([]byte("ok"))
	check(rec, http.StatusOK, "implicit 200")
	// mux 默认 404 路径端到端。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	rec = httptest.NewRecorder()
	mux.ServeHTTP(&secureHeaderWriter{ResponseWriter: rec}, httptest.NewRequest(http.MethodGet, "/nope", nil))
	check(rec, http.StatusNotFound, "mux 404")
	// mux 默认 405 路径端到端。
	rec = httptest.NewRecorder()
	mux.ServeHTTP(&secureHeaderWriter{ResponseWriter: rec}, httptest.NewRequest(http.MethodPost, "/x", nil))
	check(rec, http.StatusMethodNotAllowed, "mux 405")
}

// TestUserTokenCLI user/token CLI 全流程（注入口令读取）：
// add → 登录态前提；disable 最后用户拒绝；enable 恢复；passwd 改密；
// token create/list/revoke。
func TestUserTokenCLI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "console.yaml")
	dbPath := filepath.Join(dir, "data", "meshconsole.db")
	if err := os.WriteFile(cfgPath, []byte("db_path: \""+dbPath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pwReader := func(prompt string) (string, error) { return "a-secure-pass-1", nil }

	// add lunge。
	if code := cmdUser([]string{"add", "lunge", "-config", cfgPath}, cfgPath, pwReader); code != 0 {
		t.Fatalf("user add exit = %d", code)
	}
	// 重复 add 拒绝。
	if code := cmdUser([]string{"add", "lunge"}, cfgPath, pwReader); code != 1 {
		t.Fatalf("dup add exit = %d, want 1", code)
	}
	// disable 唯一用户拒绝（最后启用用户保护，退出码 1）。
	if code := cmdUser([]string{"disable", "lunge"}, cfgPath, pwReader); code != 1 {
		t.Fatalf("disable last exit = %d, want 1", code)
	}
	// token create → 拿到明文。
	if code := cmdToken([]string{"create", "lunge", "-desc", "hana", "-config", cfgPath}, cfgPath); code != 0 {
		t.Fatalf("token create exit = %d", code)
	}
	// passwd（注入新口令）。
	if code := cmdUser([]string{"passwd", "lunge", "-config", cfgPath}, cfgPath, func(string) (string, error) {
		return "another-secure-2", nil
	}); code != 0 {
		t.Fatalf("user passwd exit = %d", code)
	}
	// passwd 后 token 全撤销：list 为空态仍 0。
	if code := cmdToken([]string{"list", "-config", cfgPath}, cfgPath); code != 0 {
		t.Fatalf("token list exit = %d", code)
	}
	// 造第二用户后 disable 第一个成功。
	if code := cmdUser([]string{"add", "second"}, cfgPath, pwReader); code != 0 {
		t.Fatalf("user add second exit = %d", code)
	}
	if code := cmdUser([]string{"disable", "lunge"}, cfgPath, pwReader); code != 0 {
		t.Fatalf("disable exit = %d", code)
	}
	// 再 disable 到最后启用用户（second）时拒绝。
	if code := cmdUser([]string{"disable", "second"}, cfgPath, pwReader); code != 1 {
		t.Fatalf("disable last-of-two exit = %d, want 1", code)
	}
	if code := cmdUser([]string{"enable", "lunge"}, cfgPath, pwReader); code != 0 {
		t.Fatalf("enable exit = %d", code)
	}
	// token revoke：用实际存在的 id（AUTOINCREMENT 不复用，逐库动态取）。
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAPITokens(t.Context())
	st.Close()
	if err != nil || len(rows) == 0 { // passwd 已撤销旧 token：重新签发一个用于 revoke
		if code := cmdToken([]string{"create", "lunge", "-config", cfgPath}, cfgPath); code != 0 {
			t.Fatalf("token create 2 exit = %d", code)
		}
		st, err = store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		rows, err = st.ListAPITokens(t.Context())
		st.Close()
		if err != nil || len(rows) != 1 {
			t.Fatalf("tokens = %d, %v", len(rows), err)
		}
	}
	id := strconv.FormatInt(rows[0].ID, 10)
	if code := cmdToken([]string{"revoke", id, "-config", cfgPath}, cfgPath); code != 0 {
		t.Fatalf("token revoke exit = %d", code)
	}
	if code := cmdToken([]string{"revoke", "999", "-config", cfgPath}, cfgPath); code != 1 {
		t.Fatalf("token revoke missing exit = %d, want 1", code)
	}
}

// TestTokenCreateFlagParseError flag 解析失败必须报错退出、不签发（R33-#5）：
// -expires 缺值或未知 flag 时，修复前解析错误被忽略、仍会签出长期 token。
func TestTokenCreateFlagParseError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "console.yaml")
	dbPath := filepath.Join(dir, "data", "meshconsole.db")
	if err := os.WriteFile(cfgPath, []byte("db_path: \""+dbPath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pwReader := func(prompt string) (string, error) { return "a-secure-pass-1", nil }
	if code := cmdUser([]string{"add", "lunge", "-config", cfgPath}, cfgPath, pwReader); code != 0 {
		t.Fatalf("user add exit = %d", code)
	}
	// -expires 缺值（尾随 flag 无参数）与未知 flag 两种形态都须 2 退出。
	for name, args := range map[string][]string{
		"missing value":   {"create", "lunge", "-expires", "-config", cfgPath},
		"unknown flag":    {"create", "lunge", "-expir", "2026-12-31T23:59:59Z", "-config", cfgPath},
		"no value at end": {"create", "lunge", "-expires"},
	} {
		if code := cmdToken(args, cfgPath); code != 2 {
			t.Fatalf("%s: exit = %d, want 2", name, code)
		}
	}
	// 库中零 token：解析失败路径不得签发任何凭据。
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAPITokens(t.Context())
	st.Close()
	if err != nil || len(rows) != 0 {
		t.Fatalf("tokens after failed parse = %d, %v; want 0", len(rows), err)
	}
	// 对照：合法参数照常签发。
	if code := cmdToken([]string{"create", "lunge", "-expires", "2026-12-31T23:59:59Z", "-config", cfgPath}, cfgPath); code != 0 {
		t.Fatalf("valid create exit = %d", code)
	}
}

// TestExtraPositionalArgsRejected 多余位置参数一律拒绝（R35-#3）：flag 包遇
// 首个位置参数即停，`token create lunge extra -expires <未来>` 修复前会在
// extra 处停止解析、-expires 被静默忽略后仍签发长期 token。各子命令按各自
// 期望的位置参数数复核，多余即退出 2 且不产生任何变更；合法形态对照正常。
func TestExtraPositionalArgsRejected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "console.yaml")
	dbPath := filepath.Join(dir, "data", "meshconsole.db")
	if err := os.WriteFile(cfgPath, []byte("db_path: \""+dbPath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pwReader := func(prompt string) (string, error) { return "a-secure-pass-1", nil }
	if code := cmdUser([]string{"add", "lunge", "-config", cfgPath}, cfgPath, pwReader); code != 0 {
		t.Fatalf("user add exit = %d", code)
	}
	// token create 多余位置参数 × 有无 -expires 两形态：均退出 2。
	for name, args := range map[string][]string{
		"bare extra":      {"create", "lunge", "extra", "-config", cfgPath},
		"extra + expires": {"create", "lunge", "extra", "-expires", "2027-06-30T23:59:59Z", "-config", cfgPath},
	} {
		if code := cmdToken(args, cfgPath); code != 2 {
			t.Fatalf("token create %s: exit = %d, want 2", name, code)
		}
	}
	// 库中零 token：拒绝路径不得落任何凭据（R34-#5 口径）。
	countTokens := func() int {
		t.Helper()
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := st.ListAPITokens(t.Context())
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if n := countTokens(); n != 0 {
		t.Fatalf("tokens after rejected create = %d, want 0", n)
	}
	// user add / disable 多余位置参数：退出 2 且口令读取不得被触发。
	if code := cmdUser([]string{"add", "lunge", "extra", "-config", cfgPath}, cfgPath,
		func(string) (string, error) {
			t.Error("readPw called on rejected parse")
			return "", fmt.Errorf("must not be called")
		}); code != 2 {
		t.Fatalf("user add extra: exit = %d, want 2", code)
	}
	if code := cmdUser([]string{"disable", "lunge", "extra"}, cfgPath, pwReader); code != 2 {
		t.Fatalf("user disable extra: exit = %d, want 2", code)
	}
	// token revoke / list 多余位置参数：退出 2。
	if code := cmdToken([]string{"revoke", "1", "extra", "-config", cfgPath}, cfgPath); code != 2 {
		t.Fatalf("token revoke extra: exit = %d, want 2", code)
	}
	if code := cmdToken([]string{"list", "extra", "-config", cfgPath}, cfgPath); code != 2 {
		t.Fatalf("token list extra: exit = %d, want 2", code)
	}
	// 对照：合法形态（含 -expires）正常签发，且到期时间确实落库。
	if code := cmdToken([]string{"create", "lunge", "-expires", "2027-06-30T23:59:59Z", "-config", cfgPath}, cfgPath); code != 0 {
		t.Fatalf("valid create exit = %d", code)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAPITokens(t.Context())
	st.Close()
	if err != nil || len(rows) != 1 {
		t.Fatalf("valid create tokens = %d, %v; want 1", len(rows), err)
	}
	if !rows[0].ExpiresAt.Valid {
		t.Fatalf("valid create -expires ignored: expires_at = NULL")
	}
}

// TestStripConfigFlagEqualsForm -config= 等号写法只剥离 config 参数本身、保留
// 其后全部参数（R37 候选核销 / R39-#5）：修复前等号形态把该参数之后的 argv
// 全部静默丢弃，-expires/-scope 被吞、多余位置参数逃过 NArg 复核，仍照签
// 长期 token。剥离纯函数 + token create 全链两形态钉住。
func TestStripConfigFlagEqualsForm(t *testing.T) {
	// 剥离纯函数：等号形态仅去该项本身；分离值形态行为不变；无 -config 原样。
	if got := stripConfigFlag([]string{"create", "lunge", "-config=/tmp/c.yaml", "-expires", "X"}); !equalArgs(got, []string{"create", "lunge", "-expires", "X"}) {
		t.Fatalf("= form strip = %v", got)
	}
	if got := stripConfigFlag([]string{"create", "-config", "/tmp/c.yaml", "lunge"}); !equalArgs(got, []string{"create", "lunge"}) {
		t.Fatalf("space form strip = %v", got)
	}
	if got := stripConfigFlag([]string{"create", "lunge"}); !equalArgs(got, []string{"create", "lunge"}) {
		t.Fatalf("no config strip = %v", got)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "console.yaml")
	dbPath := filepath.Join(dir, "data", "meshconsole.db")
	if err := os.WriteFile(cfgPath, []byte("db_path: \""+dbPath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pwReader := func(prompt string) (string, error) { return "a-secure-pass-1", nil }
	if code := cmdUser([]string{"add", "lunge", "-config", cfgPath}, cfgPath, pwReader); code != 0 {
		t.Fatalf("user add exit = %d", code)
	}
	// 形态①：`-config=x` 之后再出现多余位置参数 + -expires —— 后缀不再被吞，
	// NArg 复核在剥离结果上生效，退出 2 且零签发。
	if code := cmdToken([]string{"create", "lunge", "-config=" + cfgPath, "extra", "-expires", "2027-06-30T23:59:59Z"}, cfgPath); code != 2 {
		t.Fatalf("= form with extra arg: exit = %d, want 2", code)
	}
	// -scope operator 后置不再被静默忽略：多余位置参数 + 后置 -scope 同拒。
	if code := cmdToken([]string{"create", "lunge", "-config=" + cfgPath, "extra", "-scope", "operator"}, cfgPath); code != 2 {
		t.Fatalf("= form with trailing -scope: exit = %d, want 2", code)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAPITokens(t.Context())
	st.Close()
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected forms issued %d tokens, %v; want 0", len(rows), err)
	}
	// 形态②：`-config=x -expires <未来>` 正常签发（-expires 在等号写法之后、
	// 必须存活到 flag 解析）。
	if code := cmdToken([]string{"create", "lunge", "-config=" + cfgPath, "-expires", "2027-06-30T23:59:59Z"}, cfgPath); code != 0 {
		t.Fatalf("= form valid create exit = %d", code)
	}
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListAPITokens(t.Context())
	st.Close()
	if err != nil || len(rows) != 1 {
		t.Fatalf("= form valid tokens = %d, %v; want 1", len(rows), err)
	}
	if !rows[0].ExpiresAt.Valid {
		t.Fatalf("= form valid create lost -expires: expires_at = NULL")
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
