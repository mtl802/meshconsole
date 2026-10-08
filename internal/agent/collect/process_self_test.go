package collect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mtl802/meshconsole/internal/config"
)

// TestProcessSelfRescue 观察点③：pgrep 报「无匹配」（exit 1）时的自进程核对。
// agent 自身命令行命中 target → active（self 兜底，附来源说明）；
// 不命中 → 维持 inactive（不编造）。
func TestProcessSelfRescue(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		if name != "pgrep" {
			t.Fatalf("must only exec pgrep, got %s", name)
		}
		return cmdResult{}, exitErr(1)
	}
	c := newTestChecker(t, []config.ServiceDecl{
		{Name: "self-hit", Type: "process", Target: "meshagent"},
		{Name: "self-miss", Type: "process", Target: "nginx"},
	}, r)
	c.selfArgs = func() []string {
		return []string{"/opt/meshconsole/bin/meshagent", "run", "-config", "/opt/meshconsole/agent.yaml"}
	}
	got := c.CheckAll(context.Background())
	if got[0].Status != "active" || !strings.Contains(got[0].Detail, "self") {
		t.Fatalf("self-hit: status=%q detail=%q, want active+self note", got[0].Status, got[0].Detail)
	}
	if got[1].Status != "inactive" {
		t.Fatalf("self-miss: status=%q, want inactive (no fabrication)", got[1].Status)
	}
}

// TestProcessSelfRescueRegex pgrep -f 为 ERE 语义：target 为正则时自核对同口径
// 匹配；非法正则退化为子串匹配（不 panic、不误报）。
func TestProcessSelfRescueRegex(t *testing.T) {
	r := &fakeRunner{}
	r.inject = func(name string, args ...string) (cmdResult, error) {
		return cmdResult{}, exitErr(1)
	}
	c := newTestChecker(t, []config.ServiceDecl{
		{Name: "re", Type: "process", Target: "bin/mesh[a-z]+ent"},
		{Name: "badre", Type: "process", Target: "mesh(unclosed"},
	}, r)
	c.selfArgs = func() []string { return []string{"./bin/meshagent", "run"} }
	got := c.CheckAll(context.Background())
	if got[0].Status != "active" {
		t.Fatalf("regex self-hit: status=%q, want active", got[0].Status)
	}
	// 非法正则 → 子串匹配：cmdline 不含 "mesh(unclosed" → inactive。
	if got[1].Status != "inactive" {
		t.Fatalf("bad-regex: status=%q, want inactive", got[1].Status)
	}
}

// TestProcessSelfRescueLive 真机回归（pgrep 缺失则跳过；Windows 无 pgrep）：
// 以测试二进制自身为探测目标——pgrep 常规路径应直接计入本进程（active）；
// 这正是观察点③场景的真机形态（目标=agent 自身）。
func TestProcessSelfRescueLive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no pgrep on windows")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available")
	}
	// 目标取测试二进制名（cmdline 里必然含它）——探活对象就是本进程。
	target := filepath.Base(os.Args[0])
	if strings.ContainsAny(target, " \t") {
		t.Skip("test binary path contains whitespace")
	}
	c := NewServiceChecker([]config.ServiceDecl{
		{Name: "self", Type: "process", Target: target},
	}, "docker")
	got := c.CheckAll(context.Background())
	if len(got) != 1 || got[0].Status != "active" {
		t.Fatalf("live self probe: got %+v, want active (pgrep or self-rescue must find this test process by %q)", got, target)
	}
}

// TestProcessSelfRescueLiveNotMatch 反向真机用例：必然不存在的目标 → inactive
// （self 核对不命中时不改判；与 M1b-a 语义一致，防兜底逻辑放过一切）。
func TestProcessSelfRescueLiveNotMatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no pgrep on windows")
	}
	c := NewServiceChecker([]config.ServiceDecl{
		{Name: "ghost", Type: "process", Target: "m1bb-definitely-not-running-xyz"},
	}, "docker")
	got := c.CheckAll(context.Background())
	if len(got) != 1 {
		t.Fatalf("unexpected results: %+v", got)
	}
	if got[0].Status != "inactive" && got[0].Status != "unknown" {
		t.Fatalf("ghost target: status=%q detail=%q, want inactive/unknown", got[0].Status, got[0].Detail)
	}
}
