package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- SPEC-M1d §3.5 会话活跃采集测试 ----
// 验收点名：会话轻解析容错（坏行跳过不报错）、30min 活跃窗口边界、daemon 过滤
// 标记（background=true 面板任务区不显示、数据保留）。

// sessionScannerFor 以注入目录与 now 构造扫描器（仅会话路径用；进程扫描走假
// runner）。known 其余名字一律指向临时根下未创建的路径（目录缺失=该 agent
// 缺席语义），保证测试封闭、绝不触碰真实会话目录。
func sessionScannerFor(t *testing.T, dirs map[string]string, now time.Time) *AgentTaskScanner {
	t.Helper()
	root := t.TempDir()
	full := map[string]string{}
	for _, n := range []string{"zcode", "codex", "claude", "gemini", "aider"} {
		if d, ok := dirs[n]; ok {
			full[n] = d
		} else {
			full[n] = filepath.Join(root, "absent", n)
		}
	}
	return scannerFor(taskFakeRunner{out: cmdResult{stdout: ""}}, "darwin", now, full)
}

func writeSession(t *testing.T, path, content string, mt time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// TestSessionScanDegradedContent 内容解析降级（R41-B2，伦哥定案 ②）：各格式
// 会话文件（真实样本形态）一律在列（活跃由 mtime 判定），topic/recent_action
// 恒为空串（M1e 按真实样本恢复解析）；started_at/last_activity 照常采集。
func TestSessionScanDegradedContent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fresh := now.Add(-time.Minute)
	// zcode rollout 真实形态：model_io 嵌套结构（request/response body 深层）。
	writeSession(t, filepath.Join(dir, "model-io-sess_a.jsonl"),
		`{"completedAt":"2026-10-08T15:58:25.307Z","type":"model_io","request":{"body":{"model":"glm"}}}`+"\n", fresh)
	// codex 真实形态：response_item.payload.content[].text 嵌套 JSONL。
	writeSession(t, filepath.Join(dir, "codex-rollout-1.jsonl"),
		`{"timestamp":"...","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"帮我排查 headscale 服务为何离线"}]}}`+"\n", fresh)
	// 历史轻解析可读的扁平形态：降级后同样置空（口径统一，不双轨）。
	writeSession(t, filepath.Join(dir, "flat.jsonl"),
		`{"type":"user","text":"扁平格式也不再解析"}`+"\n", fresh)

	s := sessionScannerFor(t, map[string]string{"zcode": dir}, now)
	sessions, trunc, ok := s.ScanSessions(context.Background())
	if !ok || trunc {
		t.Fatalf("ok=%v trunc=%v", ok, trunc)
	}
	if len(sessions) != 3 {
		t.Fatalf("sessions = %+v, want 3（格式无关，mtime 活跃即在列）", sessions)
	}
	for _, ss := range sessions {
		if ss.Topic != "" || ss.RecentAction != "" {
			t.Fatalf("degraded topic/recent_action must be empty: %+v", ss)
		}
		if ss.AgentName != "zcode" || ss.LastActivity != fresh.Unix() {
			t.Fatalf("session metadata: %+v", ss)
		}
		if ss.StartedAt <= 0 {
			t.Fatalf("started_at = %d（darwin birth time 或回退 mtime，须 > 0）", ss.StartedAt)
		}
	}
}

// TestSessionActiveWindowBoundary 30min 活跃窗口边界（SPEC：mtime < 30min 为
// 活跃）：窗口内 1s 活跃；恰在 30min 整点不活跃；30min+1s 不活跃。
func TestSessionActiveWindowBoundary(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"fresh 1s", -time.Second, true},
		{"29:59", -(SessionActiveWindow - time.Second), true},
		{"exactly 30:00", -SessionActiveWindow, false},
		{"30:01", -(SessionActiveWindow + time.Second), false},
	}
	for i, c := range cases {
		mt := now.Add(c.age)
		writeSession(t, filepath.Join(dir, "s"+itoa(i)+".jsonl"), "{\"text\":\"x\"}\n", mt)
	}
	s := sessionScannerFor(t, map[string]string{"zcode": dir}, now)
	sessions, _, ok := s.ScanSessions(context.Background())
	if !ok {
		t.Fatal("scan failed")
	}
	if len(sessions) != 2 {
		t.Fatalf("active sessions = %d, want 2（窗口内 2 个）: %+v", len(sessions), sessions)
	}
	for _, ss := range sessions {
		if !strings.HasSuffix(ss.SessionFile, "s0.jsonl") && !strings.HasSuffix(ss.SessionFile, "s1.jsonl") {
			t.Fatalf("unexpected active session %q（窗口外文件不得出现）", ss.SessionFile)
		}
	}
}

// TestSessionScanFormatChangeShowsEmpty 未知/损坏内容容错（降级口径的兜底钉）：
// 全坏行/空文件/未知结构 → 会话仍在列、主题/动作显示空、不报错。
func TestSessionScanFormatChangeShowsEmpty(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fresh := now.Add(-time.Minute)
	writeSession(t, filepath.Join(dir, "garbage.jsonl"), "\x00\x01 not json\n[[[\n\n", fresh)
	writeSession(t, filepath.Join(dir, "unknown.jsonl"), `{"message":{"content":"嵌套结构不认识"}}`+"\n", fresh)
	writeSession(t, filepath.Join(dir, "empty.jsonl"), "", fresh)

	s := sessionScannerFor(t, map[string]string{"codex": dir}, now)
	sessions, _, ok := s.ScanSessions(context.Background())
	if !ok {
		t.Fatal("scan failed")
	}
	if len(sessions) != 3 {
		t.Fatalf("sessions = %d, want 3（mtime 活跃即上报，内容解析不出显示空）", len(sessions))
	}
	for _, ss := range sessions {
		if ss.Topic != "" || ss.RecentAction != "" {
			t.Fatalf("unknown formats must show empty: %+v", ss)
		}
	}
}

// TestSessionScanCap 活跃会话数触顶 64：清单停在上限、truncated 如实（不冒充
// 完整清单——console 全量替换会静默吞掉第 65 个起）。
func TestSessionScanCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fresh := now.Add(-time.Minute)
	for i := 0; i < MaxAgentSessions+5; i++ {
		writeSession(t, filepath.Join(dir, "s"+itoa(i)+".jsonl"), "{\"text\":\"x\"}\n", fresh)
	}
	s := sessionScannerFor(t, map[string]string{"zcode": dir}, now)
	sessions, trunc, ok := s.ScanSessions(context.Background())
	if !ok {
		t.Fatal("scan failed")
	}
	if len(sessions) != MaxAgentSessions {
		t.Fatalf("sessions = %d, want cap %d", len(sessions), MaxAgentSessions)
	}
	if !trunc {
		t.Fatal("cap overflow must be reported truncated")
	}
}

// TestSessionScanCtxCanceled ctx 已取消：ok=false 整轮未知（不外泄半程清单）。
func TestSessionScanCtxCanceled(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, filepath.Join(dir, "s.jsonl"), "{\"text\":\"x\"}\n", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := sessionScannerFor(t, map[string]string{"zcode": dir}, time.Now())
	if _, _, ok := s.ScanSessions(ctx); ok {
		t.Fatal("canceled ctx must abort session scan")
	}
}

// TestScanProcessesBackgroundFilter daemon 过滤（SPEC-M1d §3.5）：elapsed > 1h
// 的进程标记 background=true（面板任务区不显示、数据保留）；≤1h 与 elapsed
// 未知（Windows 兜底 0）不标记。
func TestScanProcessesBackgroundFilter(t *testing.T) {
	out := `  PID   ELAPSED %CPU %MEM COMMAND
  201  25:00:01  1.0  1.0 codex app-server
  202     59:59  1.0  1.0 zcode run
  203  01:00:00  1.0  1.0 codex exec
  204  2-03:00:00  9.0  7.0 zcode serve`
	s := scannerFor(taskFakeRunner{out: cmdResult{stdout: out}}, "linux", time.Now(), nil)
	res := s.Scan(context.Background())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	byPID := map[int64]AgentTask{}
	for _, tk := range res.Tasks {
		byPID[tk.PID] = tk
	}
	if !byPID[201].Background {
		t.Fatalf("25h daemon must be background: %+v", byPID[201])
	}
	if byPID[202].Background {
		t.Fatalf("59m59s still a task: %+v", byPID[202])
	}
	if byPID[203].Background {
		t.Fatalf("exactly 1h boundary is not background（严格大于）: %+v", byPID[203])
	}
	if !byPID[204].Background {
		t.Fatalf("2d daemon must be background: %+v", byPID[204])
	}
}
