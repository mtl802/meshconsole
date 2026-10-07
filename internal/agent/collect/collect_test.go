package collect

import (
	"context"
	"encoding/json"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSnapshotJSONNullSemantics(t *testing.T) {
	// 采集失败的字段必须序列化为 null（不是 0），DESIGN §4.1 unavailable 语义。
	b, err := json.Marshal(&Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{"cpu_pct", "mem_used", "mem_total", "disk_used",
		"disk_total", "net_rx", "net_tx", "uptime_s", "load1"} {
		if !strings.Contains(s, `"`+key+`":null`) {
			t.Fatalf("field %s not null in JSON: %s", key, s)
		}
	}
	// 不得有任何字段落成 0 值冒充采集成功。
	if matched, _ := regexp.MatchString(`:(0|0\.0+)[,}]`, s); matched {
		t.Fatalf("found zero value substituting unavailable metric: %s", s)
	}
}

// fieldErrorPairs 校验「字段为 nil ⟺ Errors 里有对应说明」的不变量。
func checkNullInvariant(t *testing.T, snap *Snapshot) {
	t.Helper()
	pairs := []struct {
		missing bool
		set     bool
		key     string
		name    string
	}{
		{snap.CPUPct == nil, snap.CPUPct != nil, "cpu_pct", "cpu_pct"},
		{snap.MemUsed == nil || snap.MemTotal == nil, snap.MemUsed != nil, "mem", "mem_used"},
		{snap.DiskUsed == nil || snap.DiskTotal == nil, snap.DiskUsed != nil, "disk", "disk_used"},
		{snap.NetRx == nil || snap.NetTx == nil, snap.NetRx != nil, "net", "net_rx"},
		{snap.UptimeS == nil, snap.UptimeS != nil, "uptime_s", "uptime_s"},
		{snap.Load1 == nil, snap.Load1 != nil, "load1", "load1"},
	}
	errs := snap.Errors
	if errs == nil {
		errs = map[string]string{}
	}
	for _, p := range pairs {
		if p.missing && errs[p.key] == "" {
			t.Fatalf("%s is nil but no error explanation recorded", p.name)
		}
		if p.set && errs[p.key] != "" {
			t.Fatalf("%s has value but error recorded: %s", p.name, errs[p.key])
		}
	}
}

func TestCollectNullSemanticsInvariant(t *testing.T) {
	c := NewCollector("/", 15*time.Second)
	// 未启动 CPU 采样循环：cpu_pct 必须为 nil 且附带说明，而不是 0。
	snap := c.Collect()
	if snap.CPUPct != nil {
		t.Fatalf("cpu_pct ready without sampling: %v", *snap.CPUPct)
	}
	checkNullInvariant(t, snap)
}

func TestCollectAsyncCPU(t *testing.T) {
	c := NewCollector("/", 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := c.Collect()
		if snap.CPUPct != nil {
			if *snap.CPUPct < 0 || *snap.CPUPct > 100 {
				t.Fatalf("cpu_pct out of range: %v", *snap.CPUPct)
			}
			checkNullInvariant(t, snap)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("cpu sample never became ready")
}

// TestCollectCPUFailureAndStaleness 采样失败/过期不得回填旧值（审查 R1-#2）：
// 三种状态都必须是 null + 错误说明，或新鲜的正常值。
func TestCollectCPUFailureAndStaleness(t *testing.T) {
	c := NewCollector("/", time.Second)

	// 最近一次采样失败 → null + 失败原因，不得沿用旧值。
	c.cpuSample.Store(&cpuSample{pct: 42, at: time.Now(), err: "boom"})
	snap := c.Collect()
	if snap.CPUPct != nil || snap.Errors["cpu_pct"] != "boom" {
		t.Fatalf("failed sample: pct=%v err=%q", snap.CPUPct, snap.Errors["cpu_pct"])
	}
	checkNullInvariant(t, snap)

	// 采样过期（超过 staleAfter 未更新）→ null + 过期说明。
	c.cpuSample.Store(&cpuSample{pct: 42, at: time.Now().Add(-time.Hour)})
	snap = c.Collect()
	if snap.CPUPct != nil || !strings.Contains(snap.Errors["cpu_pct"], "stale") {
		t.Fatalf("stale sample: pct=%v err=%q", snap.CPUPct, snap.Errors["cpu_pct"])
	}
	checkNullInvariant(t, snap)

	// 新鲜且成功 → 正常值。
	c.cpuSample.Store(&cpuSample{pct: 42, at: time.Now()})
	snap = c.Collect()
	if snap.CPUPct == nil || *snap.CPUPct != 42 {
		t.Fatalf("fresh sample: pct=%v", snap.CPUPct)
	}
	if len(snap.Errors) != 0 {
		t.Fatalf("fresh sample should have no cpu error: %v", snap.Errors)
	}
}

// TestLoadPlatformGate Windows 必须显式报不可用，非 Windows 平台正常采集（审查 R1-#1）。
func TestLoadPlatformGate(t *testing.T) {
	if msg := loadNotAvailable("windows"); msg == "" {
		t.Fatal("windows must report load unavailable")
	}
	if runtime.GOOS != "windows" {
		if msg := loadNotAvailable(runtime.GOOS); msg != "" {
			t.Fatalf("platform %s should collect load, got %q", runtime.GOOS, msg)
		}
	}
}

// TestTruncErr 错误说明有界且不含控制字符。
func TestTruncErr(t *testing.T) {
	long := strings.Repeat("e", 5000)
	got := truncErr(long)
	if len(got) > 200 {
		t.Fatalf("truncErr len = %d, want ≤200", len(got))
	}
	dirty := "a\x00b\x1bc\td"
	if got := truncErr(dirty); strings.ContainsAny(got, "\x00\x1b") || !strings.Contains(got, "\t") {
		t.Fatalf("truncErr control chars: %q", got)
	}
}
