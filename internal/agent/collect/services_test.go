package collect

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
)

// fakeRunner 按需注入 canned 输出/错误，模拟 systemctl/docker/pgrep。
type fakeRunner struct {
	calls   []string
	lastCtx context.Context
	inject  func(name string, args ...string) (cmdResult, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (cmdResult, error) {
	f.calls = append(f.calls, name+" "+joinArgs(args...))
	f.lastCtx = ctx
	if f.inject != nil {
		return f.inject(name, args...)
	}
	return cmdResult{}, nil
}

func joinArgs(args ...string) string {
	s := ""
	for _, a := range args {
		s += "|" + a
	}
	return s
}

// exitErr 模拟 exec.ExitError（checkSystemd/checkProcess 只关心 ExitCode()）。
func exitErr(code int) error {
	return &fakeExitError{code: code}
}

type fakeExitError struct{ code int }

func (e *fakeExitError) Error() string { return "exit status " + strconv.Itoa(e.code) }
func (e *fakeExitError) ExitCode() int { return e.code }

// out 制造命令结果。
func out(stdout string) cmdResult { return cmdResult{stdout: stdout} }

func newTestChecker(t *testing.T, decls []config.ServiceDecl, r cmdRunner) *ServiceChecker {
	t.Helper()
	c := NewServiceChecker(decls, "docker")
	c.runner = r
	return c
}

// TestSystemdStatuses systemctl is-active 退出码 + 文本 → 状态映射（R10-#3）：
// 0=active、3=inactive（failed 文本精化为 failed）、其他非零按文本映射、
// 执行器错误（命令不存在等非 ExitError）才 unavailable。
func TestSystemdStatuses(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		if args[0] != "is-active" {
			t.Fatalf("must only run is-active, got %v", args)
		}
		switch args[1] {
		case "active-unit":
			return out("active\n"), nil
		case "inactive-unit":
			// 真实 systemctl：inactive 退出码 3（旧 mock 以 err=nil 掩盖过缺陷）。
			return out("inactive\n"), exitErr(3)
		case "failed-unit":
			// 现代 systemd：failed 单元退出码也是 3，靠文本精化。
			return out("failed\n"), exitErr(3)
		case "activating":
			// is-active 对 activating 返回 0，文本为中间态 → unknown。
			return out("activating\n"), nil
		case "unknown-unit":
			// unit 不存在：退出码 4 + 文本 inactive（部分 systemd 版本行为）。
			return out("inactive\n"), exitErr(4)
		case "dbus-err":
			return out(""), exitErr(1)
		case "empty-unit":
			return out(""), nil
		case "missing":
			return cmdResult{}, errors.New(`exec: "systemctl": executable file not found in $PATH`)
		default:
			return cmdResult{}, nil
		}
	}
	c := newTestChecker(t, []config.ServiceDecl{
		{Name: "a", Type: "systemd", Target: "active-unit"},
		{Name: "b", Type: "systemd", Target: "inactive-unit"},
		{Name: "c", Type: "systemd", Target: "failed-unit"},
		{Name: "d", Type: "systemd", Target: "activating"},
		{Name: "e", Type: "systemd", Target: "unknown-unit"},
		{Name: "f", Type: "systemd", Target: "dbus-err"},
		{Name: "g", Type: "systemd", Target: "empty-unit"},
		{Name: "h", Type: "systemd", Target: "missing"},
	}, r)
	got := c.CheckAll(context.Background())
	want := []struct {
		name, status string
	}{
		{"a", "active"}, {"b", "inactive"}, {"c", "failed"},
		{"d", "unknown"}, {"e", "inactive"}, {"f", "unknown"},
		{"g", "unknown"}, {"h", "unavailable"},
	}
	for i, w := range want {
		if got[i].Status != w.status {
			t.Fatalf("%s (%s): status = %q, want %q (detail=%q)", w.name, got[i].Target, got[i].Status, w.status, got[i].Detail)
		}
	}
	if got[7].Detail == "" {
		t.Fatal("unavailable must carry detail")
	}
	if got[1].Status != "inactive" && got[1].Detail != "" {
		t.Fatalf("inactive must not carry error detail: %+v", got[1])
	}
	// 固定 argv 防注入核对：is-active 后只跟 target。
	if r.calls[0] != "systemctl |is-active|active-unit" {
		t.Fatalf("argv wrong: %q", r.calls[0])
	}
}

// TestDockerStatuses docker ps 只读查询 → 状态映射；daemon 不可达 → unavailable；
// 二进制走配置的 docker_bin（R10-#4）。
func TestDockerStatuses(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		if name != "/usr/local/bin/docker-wrapper" {
			t.Fatalf("must exec configured docker bin, got %s", name)
		}
		// 固定 argv 核对：ps --filter name=<target> --format {{.State}}。
		if len(args) != 5 || args[0] != "ps" || args[1] != "--filter" || args[3] != "--format" || args[4] != "{{.State}}" {
			t.Fatalf("docker argv wrong: %v", args)
		}
		switch args[2] {
		case "name=derp":
			return out("running\n"), nil
		case "name=stopped":
			return out(""), nil
		case "name=weird":
			return out("paused\n"), nil
		case "name=dead-daemon":
			return cmdResult{stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock"}, errors.New("exit status 1")
		default:
			return cmdResult{}, nil
		}
	}
	c := NewServiceChecker([]config.ServiceDecl{
		{Name: "derp", Type: "docker", Target: "derp"},
		{Name: "stopped", Type: "docker", Target: "stopped"},
		{Name: "weird", Type: "docker", Target: "weird"},
		{Name: "dead", Type: "docker", Target: "dead-daemon"},
	}, "/usr/local/bin/docker-wrapper")
	c.runner = r
	got := c.CheckAll(context.Background())
	want := []struct {
		name, status string
	}{
		{"derp", "active"}, {"stopped", "inactive"}, {"weird", "unknown"}, {"dead", "unavailable"},
	}
	for i, w := range want {
		if got[i].Status != w.status {
			t.Fatalf("%s: status = %q, want %q (detail=%q)", w.name, got[i].Status, w.status, got[i].Detail)
		}
	}
	if got[3].Detail == "" || !contains(got[3].Detail, "daemon") {
		t.Fatalf("docker daemon error must surface in detail: %q", got[3].Detail)
	}
}

// TestProcessStatuses pgrep 计数语义：exit 0 有行=active、exit 1=inactive、
// exit ≥2=unknown、二进制缺失=unavailable。
func TestProcessStatuses(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		if name != "pgrep" {
			t.Fatalf("must only exec pgrep, got %s", name)
		}
		if args[0] != "-f" {
			t.Fatalf("pgrep must use -f, got %v", args)
		}
		switch args[1] {
		case "running-proc":
			return out("123\n456\n"), nil
		case "no-match":
			return cmdResult{}, exitErr(1)
		case "syserr":
			return cmdResult{}, exitErr(2)
		case "zero-without-output":
			return cmdResult{}, nil
		default:
			return cmdResult{}, errors.New(`exec: "pgrep": executable file not found in $PATH`)
		}
	}
	c := newTestChecker(t, []config.ServiceDecl{
		{Name: "p1", Type: "process", Target: "running-proc"},
		{Name: "p2", Type: "process", Target: "no-match"},
		{Name: "p3", Type: "process", Target: "syserr"},
		{Name: "p4", Type: "process", Target: "zero-without-output"},
		{Name: "p5", Type: "process", Target: "no-pgrep"},
	}, r)
	got := c.CheckAll(context.Background())
	want := []struct {
		name, status string
	}{
		{"p1", "active"}, {"p2", "inactive"}, {"p3", "unknown"},
		{"p4", "unknown"}, {"p5", "unavailable"},
	}
	for i, w := range want {
		if got[i].Status != w.status {
			t.Fatalf("%s: status = %q, want %q (detail=%q)", w.name, got[i].Status, w.status, got[i].Detail)
		}
	}
	if got[0].Detail != "count=2" {
		t.Fatalf("process count detail = %q, want count=2", got[0].Detail)
	}
}

// TestCheckAllIsolation 单条失败不影响其他条。
func TestCheckAllIsolation(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		if args[len(args)-1] == "broken" {
			return cmdResult{}, errors.New("boom")
		}
		return out("active\n"), nil
	}
	c := newTestChecker(t, []config.ServiceDecl{
		{Name: "good1", Type: "systemd", Target: "good1"},
		{Name: "broken", Type: "systemd", Target: "broken"},
		{Name: "good2", Type: "systemd", Target: "good2"},
	}, r)
	got := c.CheckAll(context.Background())
	if got[0].Status != "active" || got[1].Status != "unavailable" || got[2].Status != "active" {
		t.Fatalf("isolation broken: %+v", got)
	}
}

func TestCheckAllEmpty(t *testing.T) {
	c := newTestChecker(t, nil, &fakeRunner{})
	if got := c.CheckAll(context.Background()); got != nil {
		t.Fatalf("no decls should report nil, got %+v", got)
	}
}

// TestCappedBuffer 写入封顶语义（R10-#5）：未触顶全量保留；触顶截断并置位标记。
func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 8}
	if n, err := b.Write([]byte("12345")); n != 5 || err != nil || b.buf.Len() != 5 || b.truncated {
		t.Fatalf("under cap write wrong: n=%d err=%v len=%d trunc=%v", n, err, b.buf.Len(), b.truncated)
	}
	// 恰好写满：不置位。
	if n, err := b.Write([]byte("678")); n != 3 || err != nil || b.truncated {
		t.Fatalf("exact cap write wrong: n=%d err=%v trunc=%v", n, err, b.truncated)
	}
	// 触顶：报告全量写入（不中断调用方），超出部分丢弃。
	if n, err := b.Write([]byte("9ABC")); n != 4 || err != nil || !b.truncated {
		t.Fatalf("over cap write wrong: n=%d err=%v trunc=%v", n, err, b.truncated)
	}
	if got := b.buf.String(); got != "12345678" {
		t.Fatalf("capped content = %q, want 12345678", got)
	}
}

// TestExecRunnerCappedOutput execRunner 真实执行：>64KB 输出在写入阶段截断并
// 置位标记（R10-#5）。依赖 POSIX sh（CI/三平台测试均在 darwin/linux 上跑）。
func TestExecRunnerCappedOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this platform")
	}
	r := execRunner{}
	res, err := r.Run(context.Background(), "sh", "-c", "i=0; while [ $i -lt 40000 ]; do printf '0123456789abcdef'; i=$((i+1)); done")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.truncated {
		t.Fatal("640KB output must be marked truncated")
	}
	if len(res.stdout) != maxExecOutput {
		t.Fatalf("stdout len = %d, want capped %d", len(res.stdout), maxExecOutput)
	}

	res, err = r.Run(context.Background(), "sh", "-c", "echo small; echo err >&2")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.truncated || strings.TrimSpace(res.stdout) != "small" || strings.TrimSpace(res.stderr) != "err" {
		t.Fatalf("small output wrong: %+v", res)
	}
}

// TestTruncatedMarkedInDetail 截断标注进 detail（R10-#5）：fakeRunner 注入
// truncated 结果，systemd/docker/process 三型的 detail 均须带 truncNote。
func TestTruncatedMarkedInDetail(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		switch name {
		case "systemctl":
			return cmdResult{stdout: "active\n", truncated: true}, nil
		case "pgrep":
			return cmdResult{stdout: "1\n", truncated: true}, nil
		default:
			return cmdResult{stdout: "running\n", truncated: true}, nil
		}
	}
	c := newTestChecker(t, []config.ServiceDecl{
		{Name: "s", Type: "systemd", Target: "u"},
		{Name: "p", Type: "process", Target: "no-such-target-xyzq"},
		{Name: "d", Type: "docker", Target: "c"},
	}, r)
	got := c.CheckAll(context.Background())
	for _, g := range got {
		if !contains(g.Detail, truncNote) {
			t.Fatalf("%s detail must note truncation, got %q", g.Name, g.Detail)
		}
		if g.Status != "active" {
			t.Fatalf("%s status = %q, want active", g.Name, g.Status)
		}
	}
	// 空 detail（如 pgrep exit 1 inactive）截断时也要有标注。
	r2 := &fakeRunner{}
	r2.inject = func(name string, args ...string) (cmdResult, error) {
		return cmdResult{truncated: true}, exitErr(1)
	}
	c2 := newTestChecker(t, []config.ServiceDecl{{Name: "p", Type: "process", Target: "no-such-target-xyzq"}}, r2)
	got2 := c2.CheckAll(context.Background())
	if got2[0].Status != "inactive" || !contains(got2[0].Detail, truncNote) {
		t.Fatalf("truncated inactive must carry note: %+v", got2[0])
	}
}

// TestScanBudgetExhausted 整轮总预算（R11-G）：预算耗尽后未执行的条目一律
// unknown + budget 说明（不编造状态）；预算充足时照常查询。
func TestScanBudgetExhausted(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		if args[len(args)-1] == "slow" {
			// 阻塞到本轮预算 ctx 到期（确定性地耗尽预算，无定时器竞态）。
			<-r.lastCtx.Done()
			return cmdResult{}, r.lastCtx.Err()
		}
		return out("active\n"), nil
	}
	decls := []config.ServiceDecl{
		{Name: "s1", Type: "systemd", Target: "fast1"},
		{Name: "s2", Type: "systemd", Target: "slow"},
		{Name: "s3", Type: "systemd", Target: "fast3"},
	}
	c := newTestChecker(t, decls, r)
	c.budget = 50 * time.Millisecond
	got := c.CheckAll(context.Background())
	if len(got) != 3 {
		t.Fatalf("all entries must still be reported, got %+v", got)
	}
	if got[0].Status != "active" {
		t.Fatalf("s1 within budget must be active: %+v", got[0])
	}
	for _, g := range got[1:] {
		if g.Status != "unknown" || !contains(g.Detail, "budget") && !contains(g.Detail, "timeout") {
			t.Fatalf("budget-exhausted entry must be unknown with budget/timeout note: %+v", g)
		}
	}
	if !contains(got[2].Detail, "budget") {
		t.Fatalf("s3 (not yet executed) must carry budget note: %+v", got[2])
	}
	// 对照：预算充足（默认 10s）→ 全部正常查询为 active（独立 runner，不阻塞）。
	r2 := &fakeRunner{}
	r2.inject = func(name string, args ...string) (cmdResult, error) {
		return out("active\n"), nil
	}
	c2 := newTestChecker(t, decls, r2)
	got2 := c2.CheckAll(context.Background())
	for _, g := range got2 {
		if g.Status != "active" {
			t.Fatalf("entry under normal budget must be active: %+v", g)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
