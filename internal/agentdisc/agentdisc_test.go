package agentdisc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
)

// writeScript 在目录下写一个可执行 shell 脚本并返回其路径。
func writeScript(t *testing.T, dir, name, body string, executable bool) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	if executable {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestPathScan PATH 探测：存在→active、不可执行→不出现、退出非零→unavailable。
// 使用确定性假名字（不依赖真实机器装了哪些 CLI），PATH 追加而非替换（脚本依赖系统工具）。
func TestPathScan(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "fake-zcode", "echo zcode 1.2.3\n", true)
	writeScript(t, bin, "fake-codex", "echo 'codex 0.9.1\nsecond line'\n", true)
	// fake-gemini 存在但不可执行：LookPath 不命中 → 不上报。
	writeScript(t, bin, "fake-gemini", "echo gemini\n", false)
	// fake-claude 输出非零退出：上报 unavailable + detail。
	writeScript(t, bin, "fake-claude", "echo crash >&2\nexit 3\n", true)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := New([]string{"fake-zcode", "fake-codex", "fake-gemini", "fake-claude", "fake-aider"}, nil, nil)
	s.VersionTimeout = 3 * time.Second
	got := s.Scan(context.Background())

	byName := map[string]Report{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if len(got) != 3 {
		t.Fatalf("want 3 discovered (non-exec/absent CLI not reported), got %+v", got)
	}
	z := byName["fake-zcode"]
	if z.Status != "active" || z.Version != "zcode 1.2.3" || z.Path != filepath.Join(bin, "fake-zcode") {
		t.Fatalf("fake-zcode wrong: %+v", z)
	}
	if byName["fake-codex"].Version != "codex 0.9.1" {
		t.Fatalf("codex version should be first line: %+v", byName["fake-codex"])
	}
	if _, ok := byName["fake-gemini"]; ok {
		t.Fatal("non-executable gemini must not be reported")
	}
	if _, ok := byName["fake-aider"]; ok {
		t.Fatal("absent aider must not be reported")
	}
	cl := byName["fake-claude"]
	if cl.Status != "unavailable" || cl.Detail == "" {
		t.Fatalf("claude should be unavailable with detail: %+v", cl)
	}
	if cl.Version != "" {
		t.Fatalf("failed probe must not report a version: %+v", cl)
	}

	// 结果按名称排序（上报稳定）。
	for i := 1; i < len(got); i++ {
		if got[i].Name < got[i-1].Name {
			t.Fatalf("results not sorted: %+v", got)
		}
	}
}

// TestVersionTimeout 版本探测超时 → unavailable + 超时说明（进程被杀，不挂扫描）。
func TestVersionTimeout(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "fake-zcode", "/bin/sleep 30\n", true)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := New([]string{"fake-zcode"}, nil, nil)
	s.VersionTimeout = 100 * time.Millisecond
	start := time.Now()
	got := s.Scan(context.Background())
	if time.Since(start) > 5*time.Second {
		t.Fatal("scan must not hang past timeout")
	}
	if len(got) != 1 || got[0].Status != "unavailable" || !strings.Contains(got[0].Detail, "timeout") {
		t.Fatalf("timeout probe wrong: %+v", got)
	}
}

// TestCustomOverridesKnown 声明优先：同名 custom 覆盖 PATH 探测结果。
func TestCustomOverridesKnown(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "zcode", "echo from-PATH 1.0\n", true)
	t.Setenv("PATH", bin)

	alt := t.TempDir()
	altCmd := writeScript(t, alt, "zcode-alt", "echo custom 2.0\n", true)

	s := New([]string{"zcode"},
		[]config.CustomAgent{{Name: "zcode", Type: "cli", Command: altCmd, VersionFlag: "--version"}},
		nil)
	s.VersionTimeout = 3 * time.Second
	got := s.Scan(context.Background())
	if len(got) != 1 {
		t.Fatalf("want single merged entry, got %+v", got)
	}
	if got[0].Version != "custom 2.0" || got[0].Path != altCmd {
		t.Fatalf("custom must override PATH result: %+v", got[0])
	}
}

// TestCustomServicePort 服务型 agent 端口探测：开=active，关=inactive。
func TestCustomServicePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	openPort := ln.Addr().(*net.TCPAddr).Port

	// 找一个大概率关闭的端口。
	closedPort := openPort + 7
	for closedPort == openPort {
		closedPort++
	}

	s := New([]string{}, nil, []config.CustomAgentService{
		{Name: "hana-open", Port: openPort},
		{Name: "hana-closed", Port: closedPort},
	})
	got := s.Scan(context.Background())
	if len(got) != 2 {
		t.Fatalf("want 2 service reports, got %+v", got)
	}
	byName := map[string]Report{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if byName["hana-open"].Status != "active" || byName["hana-open"].Type != "service" {
		t.Fatalf("open port should be active: %+v", byName["hana-open"])
	}
	if byName["hana-closed"].Status != "inactive" || byName["hana-closed"].Detail == "" {
		t.Fatalf("closed port should be inactive with detail: %+v", byName["hana-closed"])
	}
}

// TestCustomCLIMissing 声明的 CLI 文件不存在 → unavailable + 说明（显式声明必须可见）。
func TestCustomCLIMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	s := New([]string{}, []config.CustomAgent{{Name: "ghost", Type: "cli", Command: missing}}, nil)
	s.VersionTimeout = time.Second
	got := s.Scan(context.Background())
	if len(got) != 1 || got[0].Status != "unavailable" || got[0].Detail == "" {
		t.Fatalf("missing custom cli wrong: %+v", got)
	}
}

// TestScanRespectsCtx ctx 取消时探测中断并如实上报，不编造 active/inactive。
func TestScanRespectsCtx(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "zcode", "sleep 30\n", true)
	t.Setenv("PATH", bin)

	ctx, cancel := context.WithCancel(context.Background())
	s := New([]string{"zcode"}, nil, nil)
	s.VersionTimeout = 10 * time.Second
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	got := s.Scan(ctx)
	for _, r := range got {
		if r.Status == "active" || r.Status == "inactive" {
			t.Fatalf("cancelled probe must not fabricate a conclusion: %+v", r)
		}
	}
}

// TestPortProbeCancel 端口探测响应 ctx 取消（R10-#6 DialContext）：探测被取消
// 时按 unavailable 上报且 detail 说明，不编造 inactive。
func TestPortProbeCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 先取消再探测：DialContext 立即返回 ctx 错误
	s := New([]string{}, nil, []config.CustomAgentService{{Name: "hana", Port: port}})
	got := s.Scan(ctx)
	if len(got) != 1 {
		t.Fatalf("want 1 report, got %+v", got)
	}
	if got[0].Status != "unavailable" || !strings.Contains(got[0].Detail, "cancel") {
		t.Fatalf("cancelled port probe must be unavailable with detail, got %+v", got[0])
	}
}

// TestVersionTruncatedSelectedLine R13-#4：截断判定针对「实际选中的行/缓冲」——
// 版本值取自哪个缓冲，就按哪个缓冲的截断状态与该行的换行完整性判断；来自
// 截断缓冲且无换行终止的行不得冒充版本值（报 unavailable + truncated 说明）。
func TestVersionTruncatedSelectedLine(t *testing.T) {
	bin := t.TempDir()
	// stdout 空行在前 + 超限单行（无换行）被截断：空行的换行不再掩盖「选中行
	// 本身是被截断的半行」（修复前 Contains("\n") 判定被空行骗过，半行照报）。
	writeScript(t, bin, "blank-then-flood", "printf '\\n\\n\\n'; head -c 70000 /dev/zero | tr '\\0' 'a'\n", true)
	// stdout 全空行 + 超限空白被截断：「无版本输出」的结论不可信（真实版本行
	// 可能已被截掉），不得静默按 active+空版本放行。
	writeScript(t, bin, "blank-flood-only", "printf '\\n\\n\\n'; head -c 70000 /dev/zero | tr '\\0' ' '\n", true)
	// stderr 超限单行（无换行）被截断、stdout 空：选中的 stderr 半行不得作版本
	// （修复前截断检查只看 stdout）。
	writeScript(t, bin, "stderr-flood", "head -c 70000 /dev/zero | tr '\\0' 'b' >&2\n", true)
	// 对照：版本行先出（换行在封顶之内）、后续 stderr 噪声被截断 → 该行完整，照报。
	writeScript(t, bin, "stderr-ok", "echo 'stderr-ok 1.2.3' >&2; head -c 70000 /dev/zero | tr '\\0' 'c' >&2\n", true)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := New([]string{"blank-then-flood", "blank-flood-only", "stderr-flood", "stderr-ok"}, nil, nil)
	s.VersionTimeout = 5 * time.Second
	got := s.Scan(context.Background())
	byName := map[string]Report{}
	for _, r := range got {
		byName[r.Name] = r
	}

	for _, name := range []string{"blank-then-flood", "blank-flood-only", "stderr-flood"} {
		r := byName[name]
		if r.Status != "unavailable" || r.Version != "" || !strings.Contains(r.Detail, "truncated") {
			t.Fatalf("%s: line from truncated buffer must not serve as version, got %+v", name, r)
		}
	}
	if ok := byName["stderr-ok"]; ok.Status != "active" || ok.Version != "stderr-ok 1.2.3" {
		t.Fatalf("newline-terminated line from truncated buffer is still trusted: %+v", ok)
	}
}

// TestVersionOutputCapped 版本输出 64KB 封顶（R10-#5）：换行在封顶之内→首行
// 版本正常；封顶吞掉换行→首行不完整，不冒充版本，报 unavailable+truncated。
func TestVersionOutputCapped(t *testing.T) {
	bin := t.TempDir()
	// 场景一：版本行先出（换行在封顶之内），后续 200KB 噪声被截断 → 首行版本完整可信。
	writeScript(t, bin, "ok-agent", "echo 'ok-agent 1.2.3'; head -c 200000 /dev/zero | tr '\\0' 'x'\n", true)
	// 场景二：128KB 单行无换行后才是版本行 → 截断吞掉换行 → 首行不完整，不可信。
	writeScript(t, bin, "flood-agent", "head -c 131072 /dev/zero | tr '\\0' 'y'; echo ' flood-agent 9.9.9'\n", true)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := New([]string{"ok-agent", "flood-agent"}, nil, nil)
	s.VersionTimeout = 5 * time.Second
	got := s.Scan(context.Background())
	byName := map[string]Report{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if ok := byName["ok-agent"]; ok.Status != "active" || ok.Version != "ok-agent 1.2.3" {
		t.Fatalf("newline-within-cap version must survive: %+v", ok)
	}
	fl := byName["flood-agent"]
	if fl.Status != "unavailable" || !strings.Contains(fl.Detail, "truncated") {
		t.Fatalf("flooded single-line output must be unavailable with truncated detail, got %+v", fl)
	}
	if fl.Version != "" {
		t.Fatalf("truncated version must not be reported: %+v", fl)
	}
}
