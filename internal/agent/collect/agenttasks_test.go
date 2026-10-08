package collect

// agenttasks collector 单测（SPEC-M1b-c §5）：进程匹配解析、etime 反推、
// 会话目录 stat（只 stat 不读内容）、Windows tasklist 兜底、失败语义。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// taskFakeRunner 注入 canned 输出（services_test 的 fakeRunner 同款，改名避重名）。
type taskFakeRunner struct {
	out cmdResult
	err error
}

func (f taskFakeRunner) Run(ctx context.Context, name string, args ...string) (cmdResult, error) {
	return f.out, f.err
}

func scannerFor(runner cmdRunner, goos string, now time.Time, dirs map[string]string) *AgentTaskScanner {
	custom := make([]struct {
		Name       string
		SessionDir string
	}, 0, len(dirs))
	for name, dir := range dirs {
		custom = append(custom, struct {
			Name       string
			SessionDir string
		}{name, dir})
	}
	s := newAgentTaskScanner([]string{"zcode", "codex", "claude", "gemini", "aider"}, custom, runner, goos, func() time.Time { return now })
	return s
}

const psOutput = `  PID   ELAPSED %CPU %MEM COMMAND
  101 01:02:03  12.5  1.3 /usr/local/bin/zcode serve --port 7700
  102     05:00   0.2  0.1 codex m1b-c review
  103  2-03:00:00  88.0  7.7 /opt/homebrew/bin/claude --resume
  104    10:00  99.9  9.9 vim ~/.codex/sessions/2026/10/08/rollout.jsonl
  105    10:00   1.0  0.5 /bin/zsh
  notapideline that must be skipped
  106 00:00:00   0.0  0.0 gemini chat`

// TestScanProcessesParse ps 输出解析：首 token 基名匹配、etime 反推 started_at、
// cpu/mem 解析、无辜进程（vim 会话文件路径含 codex）不误报、表头/残行跳过。
func TestScanProcessesParse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: psOutput}}, "darwin", now, nil)
	tasks, activity, _, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil {
		t.Fatal("activity map must never be nil")
	}
	byAgent := map[string]AgentTask{}
	for _, tk := range tasks {
		if _, dup := byAgent[tk.AgentName]; dup {
			t.Fatalf("duplicate agent in one scan: %+v", tasks)
		}
		byAgent[tk.AgentName] = tk
	}
	if len(tasks) != 4 {
		t.Fatalf("tasks = %+v, want 4 (zcode/codex/claude/gemini；vim 与 zsh 不匹配)", tasks)
	}
	zc := byAgent["zcode"]
	if zc.PID != 101 || zc.ElapsedS != 3723 {
		t.Fatalf("zcode = %+v", zc)
	}
	// 01:02:03 = 3723s；started_at = now - etime。
	if zc.StartedAt != now.Unix()-3723 {
		t.Fatalf("zcode started_at = %d, want %d", zc.StartedAt, now.Unix()-3723)
	}
	if zc.CPUPct == nil || *zc.CPUPct != 12.5 || zc.MemPct == nil || *zc.MemPct != 1.3 {
		t.Fatalf("zcode pct = %+v %+v", zc.CPUPct, zc.MemPct)
	}
	if !strings.HasPrefix(zc.Cmd, "/usr/local/bin/zcode serve") {
		t.Fatalf("zcode cmd = %q", zc.Cmd)
	}
	cd := byAgent["codex"]
	if cd.PID != 102 || cd.ElapsedS != 300 {
		t.Fatalf("codex = %+v", cd)
	}
	cl := byAgent["claude"]
	// 2-03:00:00 = 2 天 3 小时 = 183600s。
	if cl.ElapsedS != 183600 {
		t.Fatalf("claude elapsed = %d, want 183600 (dd-hh:mm:ss)", cl.ElapsedS)
	}
	gm := byAgent["gemini"]
	// etime 00:00:00 → elapsed 0 → started_at 缺席（0 = 未知，不编造）。
	if gm.ElapsedS != 0 || gm.StartedAt != 0 {
		t.Fatalf("gemini = %+v, want elapsed 0 / started_at 0", gm)
	}
}

// TestScanCmdTruncatedRuneSafe cmd 200 字节截断且不切断多字节 rune。
func TestScanCmdTruncatedRuneSafe(t *testing.T) {
	long := "/usr/local/bin/zcode run " + strings.Repeat("界", 300)
	out := "  PID   ELAPSED %CPU %MEM COMMAND\n    9    01:00   1 1 " + long
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: out}}, "linux", time.Unix(1_800_000_000, 0), nil)
	tasks, _, _, err := s.Scan(context.Background())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%v err=%v", tasks, err)
	}
	if len(tasks[0].Cmd) > MaxTaskCmdLen {
		t.Fatalf("cmd len = %d, want ≤%d", len(tasks[0].Cmd), MaxTaskCmdLen)
	}
	// rune 边界：解码必须成功且无残缺字节。
	if strings.ContainsRune(tasks[0].Cmd[len(tasks[0].Cmd)-3:], 0xFFFD) {
		t.Fatalf("cmd cut mid-rune: %q", tasks[0].Cmd[len(tasks[0].Cmd)-10:])
	}
}

// TestScanProcessFailure 返回 error（调用方据此缺席字段 + collect_errors，
// 绝不能把「扫描失败」当「没有任务」清空 console 侧）；活动度仍独立返回。
func TestScanProcessFailure(t *testing.T) {
	s := scannerFor(taskFakeRunner{err: errors.New("ps: permission denied")}, "linux", time.Now(), nil)
	_, activity, _, err := s.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want ps failure", err)
	}
	if activity == nil {
		t.Fatal("activity must be returned even when process scan fails")
	}
}

// TestScanOutputTruncatedIsError ps 输出触顶截断必须报错（真机回归：macOS
// 558 进程全量 ps ≈150KB，复用 64KB 级上限会静默丢掉高 pid 进程行）——截断点
// 之后任务已丢失，硬扫会拿不完整清单错误清行，缺席字段才是诚实语义。
func TestScanOutputTruncatedIsError(t *testing.T) {
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: "33793 01:00 1 1 zcode 300", truncated: true}}, "linux", time.Now(), nil)
	_, _, _, err := s.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want truncation error", err)
	}
}

// TestScanWindowsTasklist Windows 兜底：镜像名匹配（去 .exe）、cpu/mem/etime/
// started_at 缺席（null/0，不编造）、CSV 千分位内存列不破坏解析。
func TestScanWindowsTasklist(t *testing.T) {
	out := `"zcode.exe","4242","Console","1","51,234 K"
"explorer.exe","987","Console","1","120,000 K"
"codex.exe","500","Console","1","9,876 K"`
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: out}}, "windows", time.Now(), nil)
	tasks, _, _, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %+v, want 2", tasks)
	}
	if tasks[0].PID != 4242 || tasks[0].AgentName != "zcode" || tasks[0].Cmd != "zcode.exe" {
		t.Fatalf("task0 = %+v", tasks[0])
	}
	if tasks[0].CPUPct != nil || tasks[0].MemPct != nil || tasks[0].ElapsedS != 0 || tasks[0].StartedAt != 0 {
		t.Fatalf("windows task must carry no fabricated dimensions: %+v", tasks[0])
	}
}

// TestScanActivity 会话目录 stat：取树内最近 mtime 与文件数；目录缺失的 agent
// 在 map 中缺席（上层如实报 null）；只 stat 不读内容。
func TestScanActivity(t *testing.T) {
	root := t.TempDir()
	zcDir := filepath.Join(root, "rollout", "2026", "10", "08")
	if err := os.MkdirAll(zcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(zcDir, "old.jsonl")
	newF := filepath.Join(zcDir, "new.jsonl")
	for _, f := range []string{old, newF} {
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// old 拨到 10 分钟前、new 拨到 1 分钟前：最近 mtime 应取更晚写入的 new。
	oldT := time.Now().Add(-10 * time.Minute)
	fresh := time.Now().Add(-time.Minute)
	if err := os.Chtimes(old, oldT, oldT); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newF, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	// codex 目录缺失 → 缺席。
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: ""}}, "linux", time.Now(), map[string]string{
		"zcode": zcDir, "codex": filepath.Join(root, "missing"),
	})
	_, act, _, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, ok := act["zcode"]
	if !ok {
		t.Fatalf("zcode activity missing: %+v", act)
	}
	if a.Files != 2 {
		t.Fatalf("files = %d, want 2", a.Files)
	}
	if diff := a.LastActivity - fresh.Unix(); diff < -2 || diff > 2 {
		t.Fatalf("last_activity = %d, want ≈%d", a.LastActivity, fresh.Unix())
	}
	if _, ok := act["codex"]; ok {
		t.Fatalf("missing dir must be absent from map (null 上游语义): %+v", act)
	}
}

// TestScanSessionFileCap 目录统计触顶即停（防御异常巨大目录拖慢心跳）。条目
// 上限为文件+目录合计（含根目录本身占 1 个条目位），触顶如实置 Truncated。
func TestScanSessionFileCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < MaxSessionEntries+100; i++ {
		if err := os.WriteFile(filepath.Join(dir, itoa(i)+".bin"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := statDirTree(context.Background(), dir)
	if !ok {
		t.Fatal("statDirTree failed")
	}
	// 根目录先于文件被访问，占掉 1 个条目位：文件停在上限-1。
	if a.Files != MaxSessionEntries-1 {
		t.Fatalf("files = %d, want cap %d-1", a.Files, MaxSessionEntries)
	}
	if !a.Truncated {
		t.Fatal("cap hit must be reported as truncated (不冒充完整统计)")
	}
}

// TestScanSessionDirCapCountsDirs 海量空目录在条目上限内收敛（R27-#2）：只数
// 文件的旧实现会把全部 8292 个空目录走完且报「未截断、0 文件」；现在目录计入
// 条目、触顶即停——遍历收敛、截断可见（Files==0 证明上限是目录命中的）。
func TestScanSessionDirCapCountsDirs(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < MaxSessionEntries+100; i++ {
		if err := os.Mkdir(filepath.Join(dir, "d"+itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := statDirTree(context.Background(), dir)
	if !ok {
		t.Fatal("statDirTree failed")
	}
	if a.Files != 0 {
		t.Fatalf("files = %d, want 0 (empty dirs only)", a.Files)
	}
	if !a.Truncated {
		t.Fatal("dir cap hit must be reported as truncated")
	}
}

// TestStatDirTreeRespectsCtx ctx 已取消时拒绝遍历（预算对该路径生效的门槛，
// R27-#2）：返回 ok=false 走「未知=缺席」语义，不产出半程结果。
func TestStatDirTreeRespectsCtx(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a, ok := statDirTree(ctx, dir); ok {
		t.Fatalf("canceled ctx must abort walk, got %+v", a)
	}
}

// flipCtx 在第 k 次 Err() 调用后翻转为已取消（context.Context 的 Err 是接口
// 方法，可注入确定性时点——真实 timer 与遍历进度竞速必然闪失）。嵌入真
// context，Deadline/Done/Value 原样透传，仅 Err 被接管。
type flipCtx struct {
	context.Context
	calls int
	k     int
}

func (c *flipCtx) Err() error {
	c.calls++
	if c.calls > c.k {
		return context.Canceled
	}
	return c.Context.Err()
}

// TestWalkDirHugeSingleDirQuotaDominates 超大单目录在配额内收敛（R29-#1 核心
// 行为验收）：单目录条目远超配额时，遍历停在配额处——读取量受条目配额支配。
// 小批量（batch=64 < 总量）强制跨多个批次：配额点落在第 10 批附近，证明配额
// 在批间/批中都能截停，而非 WalkDir 式「先整读排序全目录再回调」（旧实现的
// 配额检查在回调里，对读取阶段无效）。触顶如实置 Truncated。
func TestWalkDirHugeSingleDirQuotaDominates(t *testing.T) {
	const entryCap, batch = 600, 64
	dir := t.TempDir()
	for i := 0; i < entryCap+6*batch; i++ { // 984 个文件 ≫ 配额 600
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)+".jsonl"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := walkDirTree(context.Background(), dir, entryCap, batch)
	if !ok {
		t.Fatal("walkDirTree failed")
	}
	// 根目录占 1 个条目位：文件停在上限-1（配额外的 384 个文件不被计数——
	// 配额处收敛）。读取量封顶由实现保证：ReadDir(64) 分批 + 配额先于下一批
	// 生效，全树至多读 配额/批 + 1 批 ≈ 640 条，而非目录全量 984 条。
	if a.Files != entryCap-1 {
		t.Fatalf("files = %d, want %d (quota must dominate single-dir reads)", a.Files, entryCap-1)
	}
	if !a.Truncated {
		t.Fatal("quota hit must be reported truncated (不冒充完整统计)")
	}
}

// TestWalkDirCtxExpiresMidWalk 遍历中途预算耗尽（R29-#1「超时如实上报」）：
// batch=10、100 文件 → 全程恰 11 次 Err()（入口 1 + 每批 1），k=5 确定性地
// 在第 6 批前翻转 → ok=false（预算内没数完 = 整树未知，缺席如实上报），且不
// 把半程统计冒充完整（Files=0/Truncated=false 的零值不会外泄）。
func TestWalkDirCtxExpiresMidWalk(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 100; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)+".jsonl"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &flipCtx{Context: context.Background(), k: 5}
	a, ok := walkDirTree(ctx, dir, MaxSessionEntries, 10)
	if ok {
		t.Fatalf("mid-walk ctx expiry must abort with ok=false, got %+v", a)
	}
	if a.Truncated || a.Files != 0 {
		t.Fatalf("aborted walk must surface no partial stats: %+v", a)
	}
}

// TestScanActivityBudgetExpires 整轮 8s 预算覆盖目录遍历路径（R27-#2）：预算
// 注入为 1ns，到 scanActivity 时 ctx 已过期 → 该 agent 活动度缺席（未知如实
// null），而不是拿未完成的遍历结果冒充。
func TestScanActivityBudgetExpires(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: ""}}, "linux", time.Now(), map[string]string{
		"zcode": dir,
	})
	s.budget = time.Nanosecond
	_, act, _, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := act["zcode"]; ok {
		t.Fatalf("expired budget must omit activity (unknown ≠ partial): %+v", act)
	}
}

// TestMatchCommandLine 匹配口径：只看首 token 基名。
func TestMatchCommandLine(t *testing.T) {
	names := []string{"zcode", "codex"}
	cases := []struct {
		cmd  string
		want string
	}{
		{"/usr/local/bin/zcode serve", "zcode"},
		{"zcode", "zcode"},
		{"./codex review", "codex"},
		{"vim ~/.codex/sessions/x", ""}, // 无辜进程不误报
		{"grep zcode /var/log/x", ""},   // 参数里出现名字不算
		{"python3 -m zcode_dev", ""},    // 名字不同（后缀）不匹配
		{"", ""},
	}
	for _, c := range cases {
		if got := matchCommandLine(c.cmd, names); got != c.want {
			t.Fatalf("matchCommandLine(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// TestParseEtime etime 各形态与非法输入（不编造，返回 0）。
func TestParseEtime(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"00:00", 0},
		{"05:00", 300},
		{"01:02:03", 3723},
		{"2-03:00:00", 183600},
		{"10-00:00:00", 864000},
		{"", 0},
		{"abc", 0},
		{"1:2", 62}, // 非零填充也接受
		{"12:34:56:78", 0},
		{"-5:00", 0},
	}
	for _, c := range cases {
		if got := parseEtime(c.in); got != c.want {
			t.Fatalf("parseEtime(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestScanEmptyNames 未配置任何 agent 名：Enabled=false，Scan 返回空清单
// （runner 据此整体缺席 agent_tasks 字段）。
func TestScanEmptyNames(t *testing.T) {
	s := newAgentTaskScanner(nil, nil, taskFakeRunner{}, "linux", time.Now)
	if s.Enabled() {
		t.Fatal("no names configured must be disabled")
	}
	tasks, _, _, err := s.Scan(context.Background())
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks=%v err=%v", tasks, err)
	}
}

// TestScanMaxTasks 任务数上限截断（防御病态环境刷爆心跳体）：清单停在上限，
// 且截断可见（truncated=true，R27-#4——静默截断会让 console 全量替换把第 65
// 个起的任务从面板上无声抹掉）。
func TestScanMaxTasks(t *testing.T) {
	var b strings.Builder
	b.WriteString("  PID ELAPSED %CPU %MEM COMMAND\n")
	for i := 1; i <= MaxAgentTasks+10; i++ {
		b.WriteString(" " + itoa(i) + " 01:00 1 1 /bin/zcode\n")
	}
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: b.String()}}, "linux", time.Now(), nil)
	tasks, _, truncated, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != MaxAgentTasks {
		t.Fatalf("tasks = %d, want cap %d", len(tasks), MaxAgentTasks)
	}
	if !truncated {
		t.Fatal(">64 matches must set truncated (list incomplete visible)")
	}

	// 恰好 64 个匹配：清单完整，不报截断。
	var b2 strings.Builder
	b2.WriteString("  PID ELAPSED %CPU %MEM COMMAND\n")
	for i := 1; i <= MaxAgentTasks; i++ {
		b2.WriteString(" " + itoa(i) + " 01:00 1 1 /bin/zcode\n")
	}
	s2 := scannerFor(taskFakeRunner{out: cmdResult{stdout: b2.String()}}, "linux", time.Now(), nil)
	tasks2, _, truncated2, err := s2.Scan(context.Background())
	if err != nil || len(tasks2) != MaxAgentTasks {
		t.Fatalf("tasks=%d err=%v", len(tasks2), err)
	}
	if truncated2 {
		t.Fatal("exact cap without overflow must not be truncated")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
