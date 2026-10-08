package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mtl802/meshconsole/internal/config"
)

// ---- M1d（SPEC-M1d §2/§3）agent 端测试：执行器 / 本地对账 / worker----
// 真机执行用 macOS 自带的 /bin/echo、/bin/sleep、/usr/bin/head（固定绝对路径，
// 与白名单同款 exec 形态），不依赖任何外部服务。

func testAgentState(t *testing.T) (*State, string) {
	t.Helper()
	dir := t.TempDir()
	st := &State{ConsoleURL: "https://127.0.0.1:7700", NodeID: 1, Name: "mac-mini",
		NodeToken: "tok", RegisteredAt: 1}
	st.SetStateFile(filepath.Join(dir, "state.json"))
	return st, dir
}

func testAgentCfg(l2 bool) *config.Agent {
	return &config.Agent{L2Enabled: l2, DockerBin: "docker", CollectIntervalS: 15,
		Services: []config.ServiceDecl{{Name: "headscale", Type: "systemd", Target: "headscale"}}}
}

// TestExecutorBasic 真执行：argv 直接 exec、合并输出、退出码。
func TestExecutorBasic(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("/bin/echo not available")
	}
	code, text, err := Execute(context.Background(), []string{"/bin/echo", "hello", "世界"}, 5, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if code == nil || *code != 0 {
		t.Fatalf("exit code = %v, want 0", code)
	}
	if !strings.HasPrefix(text, "hello 世界") {
		t.Fatalf("output = %q", text)
	}
}

// TestExecutorTimeout 超时 + 进程组 kill：文本带超时标注、退出码缺失。
func TestExecutorTimeout(t *testing.T) {
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("/bin/sleep not available")
	}
	start := time.Now()
	code, text, err := Execute(context.Background(), []string{"/bin/sleep", "30"}, 1, t.TempDir())
	if err != errExecTimeout {
		t.Fatalf("err = %v, want errExecTimeout", err)
	}
	if code != nil {
		t.Fatalf("timeout exit code = %v, want nil", code)
	}
	if !strings.Contains(text, "执行超时") {
		t.Fatalf("timeout note missing: %q", text)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took too long: %v (进程组 kill 未生效)", time.Since(start))
	}
}

// TestExecutorOutputCapBudget 64KB 预算含标注：最终文本总长不得超硬上限
// （console 对回执执行同等校验，超限即拒——E2E 曾因此丢回执）。
func TestExecutorOutputCapBudget(t *testing.T) {
	head := "/usr/bin/head"
	if _, err := os.Stat(head); err != nil {
		t.Skip("/usr/bin/head not available")
	}
	_, text, err := Execute(context.Background(),
		[]string{head, "-c", "200000", "/dev/zero"}, 10, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > ResultHardCap {
		t.Fatalf("final text %d bytes exceeds hard cap %d（console 将拒绝该回执）", len(text), ResultHardCap)
	}
	if !strings.Contains(text, "已截断") {
		t.Fatalf("truncation note missing")
	}
}

// TestExecutorOutputCap 64KB 硬上限截断。
func TestExecutorOutputCap(t *testing.T) {
	head := "/usr/bin/head"
	if _, err := os.Stat(head); err != nil {
		t.Skip("/usr/bin/head not available")
	}
	code, text, err := Execute(context.Background(),
		[]string{head, "-c", "200000", "/dev/zero"}, 10, t.TempDir())
	if err != nil || code == nil || *code != 0 {
		t.Fatalf("head: code=%v err=%v", code, err)
	}
	if len(text) > ResultHardCap+256 {
		t.Fatalf("output not capped: %d bytes", len(text))
	}
	if !strings.Contains(text, "已截断") {
		t.Fatalf("truncation note missing")
	}
}

// TestFinalizeResultBudgetBothForms 64KB 硬保证两形态（R39-#3）：① 超长正文
// 压缩后追加标注仍 ≤ cap（标注计入预算）；② 非法 UTF-8 密集输入替换扩容后
// 复核再压正文，仍 ≤ cap 且保持合法 UTF-8。出口长度由构造保证。
func TestFinalizeResultBudgetBothForms(t *testing.T) {
	// ① 压正文保标注：70KB ASCII + 截断标注 → 总长恰为 cap、标注在场。
	body := strings.Repeat("a", 70<<10)
	out := finalizeResult(body, true, "")
	if len(out) != ResultHardCap {
		t.Fatalf("form1: len = %d, want exactly %d", len(out), ResultHardCap)
	}
	if !strings.Contains(out, "已截断") || !strings.HasSuffix(out, truncNoteBody) {
		t.Fatalf("form1: truncation note missing/misplaced")
	}
	// ② 非法 UTF-8 替换扩容：「合法字节夹非法字节」形态下每个非法 run 独立
	// 替换为 U+FFFD（1→3 字节，1.5 倍扩容）——48KB 输入替换后 96KB，复核后
	// 压正文保标注；产物合法 UTF-8 且 ≤ cap（JSON 序列化不再扩容）。
	bad := strings.Repeat("a\xff", 24<<10)
	out = finalizeResult(bad, false, "")
	if len(out) > ResultHardCap {
		t.Fatalf("form2: len = %d exceeds cap %d", len(out), ResultHardCap)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("form2: output not valid UTF-8")
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("form2: compression note missing")
	}
	// ③ 恰满边界：body 与标注合计恰为 cap → 原样保留，无多余标注。
	exact := strings.Repeat("b", ResultHardCap-len(truncNoteBody))
	out = finalizeResult(exact, true, "")
	if len(out) != ResultHardCap || strings.Count(out, "已截断") != 1 {
		t.Fatalf("form3: exact-budget case altered: len=%d", len(out))
	}
	// ④ 防御分支：标注自身超限（正常触不到）→ 硬上限仍成立。
	hugeNote := strings.Repeat("n", ResultHardCap+10)
	out = finalizeResult("x", false, hugeNote)
	if len(out) > ResultHardCap {
		t.Fatalf("form4: len = %d exceeds cap", len(out))
	}
	// ⑤ rune 边界：多字节正文压缩后保持合法 UTF-8。
	multi := strings.Repeat("世", 30000) // 90KB 合法多字节
	out = finalizeResult(multi, false, "")
	if len(out) > ResultHardCap || !utf8.ValidString(out) {
		t.Fatalf("form5: len=%d valid=%v", len(out), utf8.ValidString(out))
	}
	// ⑥ codex 复审点名的精确形态（R40-#3）：正文恰满 65536B ASCII + 超时标注
	// （already=false，即 limitedBuffer 恰好装满后再走超时路径）——预算内压正文
	// 保住超时标注，终长恰为 cap、超时说明在场。
	full := strings.Repeat("a", ResultHardCap)
	out = finalizeResult(full, false, "\n[meshagent] 执行超时（300s），已终止进程组")
	if len(out) != ResultHardCap {
		t.Fatalf("form6: len = %d, want exactly %d（65585B 形态必须被预算拦下）", len(out), ResultHardCap)
	}
	if !strings.Contains(out, "执行超时") {
		t.Fatalf("form6: timeout note must survive the budget cut")
	}
	// ⑦ 同形态 + already=true（65536B 正文 + 截断标注已声明）：不得二次追加
	// 标注把总长顶破 cap。
	out = finalizeResult(full, true, "")
	if len(out) != ResultHardCap {
		t.Fatalf("form7: len = %d, want exactly %d", len(out), ResultHardCap)
	}
	if strings.Count(out, "已截断") != 1 {
		t.Fatalf("form7: truncation note must appear exactly once")
	}
}

// TestFinalizeResultStartFailureNoteBounded worker 启动失败标注走预算口径：
// 已满 64KB 的正文追加失败说明后仍 ≤ cap（R39-#3 同类穿透点的收口）。
func TestFinalizeResultStartFailureNoteBounded(t *testing.T) {
	full := finalizeResult(strings.Repeat("a", 200<<10), true, "")
	if len(full) != ResultHardCap {
		t.Fatalf("precondition: len = %d", len(full))
	}
	text := finalizeResult(full, false, "\n[meshagent] 启动失败：exec /bin/nope: no such file or directory")
	if len(text) > ResultHardCap {
		t.Fatalf("start-failure note overflowed: %d bytes", len(text))
	}
	if !strings.Contains(text, "启动失败") {
		t.Fatalf("start-failure note missing")
	}
}

// TestCommandStoreIntentResultAck 本地对账生命周期：intent 落盘（幂等不覆盖）→
// 结果覆盖 → ACK 删除；重启恢复（LoadAll）后未 ACK 结果仍在。对账文件按
// (command_id, lease_epoch) 命名（R41-B1）。
func TestCommandStoreIntentResultAck(t *testing.T) {
	_, dir := testAgentState(t)
	cs, err := NewCommandStore(filepath.Join(dir, "state.json")) // 入参为 state 文件路径
	if err != nil {
		t.Fatal(err)
	}
	rec := &CommandRecord{CommandID: "c-1", LeaseEpoch: 1, Kind: "ps_snapshot",
		ArgsJSON: "{}", TimeoutS: 30, ClaimedAt: 123}
	if err := cs.SaveIntent(rec); err != nil {
		t.Fatal(err)
	}
	// intent 幂等：重复保存不覆盖。
	if err := cs.SaveIntent(&CommandRecord{CommandID: "c-1", LeaseEpoch: 1, Kind: "TAMPER"}); err != nil {
		t.Fatal(err)
	}
	got, err := cs.Load("c-1", 1)
	if err != nil || got.Kind != "ps_snapshot" || got.Phase != PhaseIntent || got.LeaseEpoch != 1 {
		t.Fatalf("intent: %+v err=%v", got, err)
	}
	// 结果覆盖。
	if err := cs.SaveResult(&CommandRecord{CommandID: "c-1", LeaseEpoch: 1,
		ExitCode: iptr(0), ResultText: "out", FinishedAt: 456}); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.Load("c-1", 1)
	if got.Phase != PhaseResult || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("result: %+v", got)
	}
	// 目录权限 0700，文件 0600；文件名绑定代次。
	if fi, err := os.Stat(filepath.Join(dir, "commands")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("store dir perm: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "commands", "c-1.e1.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record file perm: %v %v", fi, err)
	}
	// ACK 删除（DeleteUpTo：≤确认代次全清）。
	if err := cs.DeleteUpTo("c-1", 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := cs.Load("c-1", 1); got != nil {
		t.Fatalf("acked record must be gone")
	}
}

// TestCommandStoreEpochIsolation 代次文件隔离（R41-B1 的存储层结构保证）：
// 同 command_id 不同代次各占一个文件；Delete 只删指定代次；DeleteUpTo 只清
// ≤上限代次、更高代次不可触达；旧版无代次文件被 PurgeLegacy 清除。
func TestCommandStoreEpochIsolation(t *testing.T) {
	_, dir := testAgentState(t)
	stateFile := filepath.Join(dir, "state.json")
	// 预置旧版残留（升级前形态：<id>.json）。
	if err := os.MkdirAll(filepath.Join(dir, "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "commands", "legacy.json"), []byte(`{"command_id":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := NewCommandStore(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := cs.PurgeLegacy()
	if err != nil || len(removed) != 1 || removed[0] != "legacy.json" {
		t.Fatalf("purge legacy: %v err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "commands", "legacy.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy file must be removed")
	}

	for _, e := range []int64{1, 2} {
		if err := cs.SaveResult(&CommandRecord{CommandID: "iso-1", LeaseEpoch: e,
			ExitCode: iptr(0), ResultText: fmt.Sprintf("gen%d", e), FinishedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// 定点删除旧代次：新代次文件原样。
	if err := cs.Delete("iso-1", 1); err != nil {
		t.Fatal(err)
	}
	if r, _ := cs.Load("iso-1", 1); r != nil {
		t.Fatalf("epoch 1 must be deleted")
	}
	if r, _ := cs.Load("iso-1", 2); r == nil || r.ResultText != "gen2" {
		t.Fatalf("epoch 2 must survive epoch-1 delete: %+v", r)
	}
	// DeleteUpTo(2) 清两代；更高代次（3）不可触达。
	if err := cs.SaveIntent(&CommandRecord{CommandID: "iso-1", LeaseEpoch: 3,
		Kind: "ps_snapshot", TimeoutS: 5}); err != nil {
		t.Fatal(err)
	}
	if err := cs.DeleteUpTo("iso-1", 2); err != nil {
		t.Fatal(err)
	}
	if r, _ := cs.Load("iso-1", 2); r != nil {
		t.Fatalf("epoch 2 must be deleted by DeleteUpTo(2)")
	}
	if r, _ := cs.Load("iso-1", 3); r == nil || r.Phase != PhaseIntent {
		t.Fatalf("epoch 3 must survive DeleteUpTo(2): %+v", r)
	}
}

func iptr(v int64) *int64 { return &v }

// TestWorkerRejects agent 端双重校验：l2 总开关关闭 / 未知 kind / unit 非受管
// ——拒绝以 failed 回执如实记录（非静默丢弃）。
func TestWorkerRejects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(false), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// l2 关闭：caps 为空（console 提交侧即拦）+ 交付拒绝留回执。
	if caps := w.Caps(); len(caps) != 0 {
		t.Fatalf("l2 disabled must report no caps, got %v", caps)
	}
	w.Deliver([]delivery{{CommandID: "d-1", Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 1, LeaseEpoch: 1}})
	waitResult(t, w, "d-1", func(r *CommandRecord) {
		if r.ExitCode != nil || !strings.Contains(r.ResultText, "l2_enabled") {
			t.Fatalf("l2 refusal: %+v", r)
		}
	})

	// l2 开启：未知 kind 拒绝。
	st2, _ := testAgentState(t)
	w2, err := NewCommandWorker(ctx, testAgentCfg(true), st2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	w2.Deliver([]delivery{{CommandID: "d-2", Kind: "rm_rf", TimeoutS: 5, ClaimedAt: 1, LeaseEpoch: 1}})
	waitResult(t, w2, "d-2", func(r *CommandRecord) {
		if !strings.Contains(r.ResultText, "未知命令类型") {
			t.Fatalf("unknown kind refusal: %+v", r)
		}
	})

	// unit 不在自身受管集合 → 拒绝。
	w2.Deliver([]delivery{{CommandID: "d-3", Kind: "systemctl_status",
		ArgsJSON: `{"unit":"nginx"}`, TimeoutS: 5, ClaimedAt: 1, LeaseEpoch: 1}})
	waitResult(t, w2, "d-3", func(r *CommandRecord) {
		if !strings.Contains(r.ResultText, "参数校验失败") {
			t.Fatalf("unmanaged unit refusal: %+v", r)
		}
	})
}

// TestWorkerExecutesAndAcks 完整执行链：受管交付 → 真执行 → 结果落盘 →
// 未 ACK 持续在册 → ACK 删除。重复交付（console 重发）不重复执行。
func TestWorkerExecutesAndAcks(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("/bin/echo not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(true), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	d := delivery{CommandID: "exec-1", Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 7, LeaseEpoch: 1}
	w.Deliver([]delivery{d, d, d}) // 重复交付 ×3
	res := waitResult(t, w, "exec-1", nil)
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("exec result: %+v", res)
	}
	if res.LeaseEpoch != 1 {
		t.Fatalf("receipt epoch = %d, want 1", res.LeaseEpoch)
	}
	// 未 ACK：持续在册（回执重发语义）。
	if snaps := w.ResultsSnapshot(); len(snaps) != 1 || snaps[0].CommandID != "exec-1" {
		t.Fatalf("unacked snapshot: %+v", snaps)
	}
	w.Ack([]ackEntry{{CommandID: "exec-1", LeaseEpoch: 1}})
	if snaps := w.ResultsSnapshot(); len(snaps) != 0 {
		t.Fatalf("acked must be removed: %+v", snaps)
	}
	if _, ok := w.generation("exec-1"); ok {
		t.Fatalf("ack must unmark the held generation")
	}
}

// TestWorkerRestartRecovery ACK 前 agent 重启恢复重发（SPEC-M1d §7 验收）：
// ① result 俱全 → 重启后仍上报（直至 ACK）；② intent 无结果 → 补 interrupted
// 结果（不重执行），console 置 unknown 对齐。
func TestWorkerRestartRecovery(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("/bin/echo not available")
	}
	_, dir := testAgentState(t)

	// 「上一世」：交付执行完成（结果落盘，未 ACK）+ 一条仅 intent（模拟半路被杀）。
	ctx1, cancel1 := context.WithCancel(context.Background())
	st1 := &State{Name: "mac-mini", NodeToken: "t"}
	st1.SetStateFile(filepath.Join(dir, "state.json"))
	w1, err := NewCommandWorker(ctx1, testAgentCfg(true), st1, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	w1.Deliver([]delivery{{CommandID: "done-1", Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 1, LeaseEpoch: 1}})
	waitResult(t, w1, "done-1", nil)
	// intent-only：直接落 intent 文件（模拟执行中途进程被杀）。
	cs, _ := NewCommandStore(filepath.Join(dir, "state.json"))
	if err := cs.SaveIntent(&CommandRecord{CommandID: "half-1", LeaseEpoch: 1, Kind: "df_report",
		TimeoutS: 5, ClaimedAt: 2}); err != nil {
		t.Fatal(err)
	}
	cancel1()

	// 「重启后」：同目录重建 worker —— done-1 结果保留重发、half-1 补 interrupted
	// 结果（非重执行）。
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	st2 := &State{Name: "mac-mini", NodeToken: "t"}
	st2.SetStateFile(filepath.Join(dir, "state.json"))
	w2, err := NewCommandWorker(ctx2, testAgentCfg(true), st2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	snaps := w2.ResultsSnapshot()
	byID := map[string]*CommandRecord{}
	for _, r := range snaps {
		byID[r.CommandID] = r
	}
	if r := byID["done-1"]; r == nil || r.ExitCode == nil || *r.ExitCode != 0 {
		t.Fatalf("done-1 must survive restart unacked: %+v", r)
	}
	if r := byID["half-1"]; r == nil || !r.Interrupted {
		t.Fatalf("half-1 must be reported interrupted: %+v", r)
	}
	// ACK 后双清。
	w2.Ack([]ackEntry{{CommandID: "done-1", LeaseEpoch: 1}, {CommandID: "half-1", LeaseEpoch: 1}})
	if snaps := w2.ResultsSnapshot(); len(snaps) != 0 {
		t.Fatalf("acked after restart must be removed: %+v", snaps)
	}
}

// TestWorkerLeaseGenerationRetire 租约代次对齐（R39-#2，R41-B1 显式代次令牌）：
// 执行超租约 → console 重领（lease_epoch 自增）→ 同一 worker 收到新代次交付。
// 旧代次静默退役（旧代次对账文件被定点删除、旧结果即使已落盘也不再上报），
// 新代次全新执行；worker 手里最终只有新代次回执，console 录入即终态可对账、
// 无双重执行。
func TestWorkerLeaseGenerationRetire(t *testing.T) {
	if _, err := os.Stat("/bin/ps"); err != nil {
		t.Skip("/bin/ps not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(true), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// 第一代（epoch=1）：正常交付执行，结果落盘待报。
	d1 := delivery{CommandID: "lease-1", Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 100, LeaseEpoch: 1}
	w.Deliver([]delivery{d1})
	r1 := waitResult(t, w, "lease-1", nil)
	if r1.LeaseEpoch != 1 {
		t.Fatalf("gen1 receipt epoch = %d, want 1", r1.LeaseEpoch)
	}
	// 重领：同 command_id、代次前移（epoch=2）再次下发。同代次重发（=1）去重
	// 跳过；新代次触发退役 + 全新执行。
	w.Deliver([]delivery{d1})
	if snaps := w.ResultsSnapshot(); len(snaps) != 1 || snaps[0].LeaseEpoch != 1 {
		t.Fatalf("same-generation redelivery must not re-execute: %+v", snaps)
	}
	d2 := d1
	d2.ClaimedAt = 200
	d2.LeaseEpoch = 2
	w.Deliver([]delivery{d2})
	// 新代次回执就位；旧代次副本不在册（不重报、不残留）。
	var r2 *CommandRecord
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snaps := w.ResultsSnapshot()
		if len(snaps) == 1 && snaps[0].LeaseEpoch == 2 {
			r2 = snaps[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r2 == nil {
		t.Fatalf("gen2 result missing or gen1 residue: %+v", w.ResultsSnapshot())
	}
	if r2.ExitCode == nil || *r2.ExitCode != 0 {
		t.Fatalf("gen2 execution result: %+v", r2)
	}
	// 稳态复核：稍候再取，清单仍只有新代次一条（旧回执永不回流）。
	time.Sleep(50 * time.Millisecond)
	if snaps := w.ResultsSnapshot(); len(snaps) != 1 || snaps[0].LeaseEpoch != 2 {
		t.Fatalf("steady snapshot: %+v", snaps)
	}
	w.Ack([]ackEntry{{CommandID: "lease-1", LeaseEpoch: 2}})
	if snaps := w.ResultsSnapshot(); len(snaps) != 0 {
		t.Fatalf("acked gen2 must be removed: %+v", snaps)
	}
}

// TestWorkerSupersededGenerationDiscard 代次退役的两条丢弃路径（R41-B1 单元
// 钉死）：① 已落盘旧代次结果——saveResultIfCurrent 复核失配不落盘；② 排队中
// 旧代次——run 入口复核失配直接返回（不执行、不落任何回执）。
func TestWorkerSupersededGenerationDiscard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(true), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// 当前代次 = 2；旧代次（1）的收尾落盘必须被静默丢弃。
	w.markStarted("sup-1", 2)
	old := &CommandRecord{CommandID: "sup-1", LeaseEpoch: 1, Kind: "ps_snapshot", TimeoutS: 5,
		ClaimedAt: 100, ExitCode: iptr(0), ResultText: "stale generation",
		FinishedAt: time.Now().Unix()}
	if w.saveResultIfCurrent(old) {
		t.Fatalf("stale generation save must be discarded")
	}
	if rec, _ := w.store.Load("sup-1", 1); rec != nil {
		t.Fatalf("stale generation result must not hit disk: %+v", rec)
	}
	// 排队中的旧代次记录（模拟重领发生在入队之后）：run 入口即退役，不执行
	// 也不落拒绝回执。
	staleQueued := &CommandRecord{CommandID: "sup-1", LeaseEpoch: 1, Kind: "ps_snapshot",
		TimeoutS: 5, ClaimedAt: 100}
	w.run(ctx, staleQueued)
	if rec, _ := w.store.Load("sup-1", 1); rec != nil {
		t.Fatalf("queued stale generation must not produce a receipt: %+v", rec)
	}
	// 当前代次照常落盘（对照）。
	cur := &CommandRecord{CommandID: "sup-1", LeaseEpoch: 2, Kind: "ps_snapshot", TimeoutS: 5,
		ClaimedAt: 200, ExitCode: iptr(0), ResultText: "current", FinishedAt: time.Now().Unix()}
	if !w.saveResultIfCurrent(cur) {
		t.Fatalf("current generation save must succeed")
	}
	if rec, _ := w.store.Load("sup-1", 2); rec == nil || rec.LeaseEpoch != 2 {
		t.Fatalf("current generation result: %+v", rec)
	}
}

// TestWorkerDropStaleEpochKeepsNewResult R41-B1 阻塞项的复现与收口（真实交错
// 时序，非先后调用）：gen1 回执已上报 → console 租约过期重领（同拍下发 gen2）
// → gen2 在途执行期间，console 对 gen1 回执的 drop 指示到达（runner 同拍
// Deliver→Drop 的交错）。旧代码按 command_id 装载唯一对账文件——装载到的已是
// gen2 的结果，误删后回执丢失、console 判 unknown 重执行；代次化后 drop 只触达
// (id, epoch=1) 一个文件，gen2 的执行与结果结构上不可被触达。
func TestWorkerDropStaleEpochKeepsNewResult(t *testing.T) {
	if _, err := os.Stat("/bin/ps"); err != nil {
		t.Skip("/bin/ps not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(true), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	const id = "b1-drop"
	// gen1：交付执行完成，回执落盘（模拟已上报、console 侧因租约过期拒收）。
	w.Deliver([]delivery{{CommandID: id, Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 1, LeaseEpoch: 1}})
	waitResult(t, w, id, nil)
	// gen2：交付触发 retire（换入代次 2），真实子进程开始执行。
	w.Deliver([]delivery{{CommandID: id, Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 2, LeaseEpoch: 2}})
	if gen, ok := w.generation(id); !ok || gen != 2 {
		t.Fatalf("gen2 must be held after delivery: gen=%d ok=%v", gen, ok)
	}
	// 旧代次 drop 与 gen2 执行真实并发：另起 goroutine 连发 drop（模拟 console
	// 多拍重发 drop 指示、与 gen2 执行任意交错）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			w.Drop([]dropEntry{{CommandID: id, LeaseEpoch: 1}})
			time.Sleep(2 * time.Millisecond)
		}
	}()
	// gen2 回执必须完好产出（若 drop 误删新代次结果，这里永远等不到）。
	r2 := waitResult(t, w, id, func(r *CommandRecord) {
		if r.LeaseEpoch != 2 {
			t.Fatalf("surviving receipt epoch = %d, want 2", r.LeaseEpoch)
		}
		if r.ExitCode == nil || *r.ExitCode != 0 || r.ResultText == "" {
			t.Fatalf("gen2 result must be intact: %+v", r)
		}
	})
	<-done
	// 稳态：只剩 gen2 一条回执，gen1 文件不存在。
	time.Sleep(30 * time.Millisecond)
	snaps := w.ResultsSnapshot()
	if len(snaps) != 1 || snaps[0].LeaseEpoch != 2 || snaps[0].ResultText != r2.ResultText {
		t.Fatalf("steady snapshot must hold exactly the gen2 receipt: %+v", snaps)
	}
	if rec, _ := w.store.Load(id, 1); rec != nil {
		t.Fatalf("stale epoch-1 record must be gone: %+v", rec)
	}
	if gen, ok := w.generation(id); !ok || gen != 2 {
		t.Fatalf("gen2 execution right must survive stale drop: gen=%d ok=%v", gen, ok)
	}
}

// TestWorkerDropAfterNewResultWritten R41-B1 的第二形态：新代次结果已落盘之后
// 旧代次 drop 才到达（console 同拍「先 Deliver 新代次快速执行完、后 Drop 旧
// 回执」的顺序化视图）——(id,1) 的定点删除不得触碰 (id,2) 结果文件。
func TestWorkerDropAfterNewResultWritten(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(true), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	const id = "b1-order"
	// 新代次（2）在途 + 新结果已写入 + 旧代次（1）回执残留（重领与 drop 同拍的
	// 交错视图：文件按代次分立，两代结果同时存在于盘上）。
	w.markStarted(id, 2)
	if err := w.store.SaveResult(&CommandRecord{CommandID: id, LeaseEpoch: 1,
		Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 100, ExitCode: iptr(0),
		ResultText: "stale receipt", FinishedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := w.store.SaveResult(&CommandRecord{CommandID: id, LeaseEpoch: 2,
		Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 200, ExitCode: iptr(0),
		ResultText: "fresh result", FinishedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	w.Drop([]dropEntry{{CommandID: id, LeaseEpoch: 1}})
	if rec, _ := w.store.Load(id, 1); rec != nil {
		t.Fatalf("stale receipt must be deleted: %+v", rec)
	}
	keep, err := w.store.Load(id, 2)
	if err != nil || keep == nil || keep.ResultText != "fresh result" || keep.Phase != PhaseResult {
		t.Fatalf("new-generation result must survive stale drop: %+v err=%v", keep, err)
	}
	if gen, ok := w.generation(id); !ok || gen != 2 {
		t.Fatalf("new generation must survive drop: gen=%d ok=%v", gen, ok)
	}
	if snaps := w.ResultsSnapshot(); len(snaps) != 1 || snaps[0].LeaseEpoch != 2 {
		t.Fatalf("snapshot must only hold the fresh receipt: %+v", snaps)
	}

	// 在途 intent 不受 drop 影响：drop 指向已上报的旧回执，intent 属于在途执行。
	w.markStarted("drop-2", 3)
	if err := w.store.SaveIntent(&CommandRecord{CommandID: "drop-2", LeaseEpoch: 3,
		Kind: "df_report", TimeoutS: 5, ClaimedAt: 300}); err != nil {
		t.Fatal(err)
	}
	w.Drop([]dropEntry{{CommandID: "drop-2", LeaseEpoch: 3}})
	if rec, _ := w.store.Load("drop-2", 3); rec == nil || rec.Phase != PhaseIntent {
		t.Fatalf("in-flight intent must survive drop: %+v", rec)
	}
	if gen, ok := w.generation("drop-2"); !ok || gen != 3 {
		t.Fatalf("in-flight generation must survive drop: gen=%d ok=%v", gen, ok)
	}
	// 同代回执的 drop 照常清场（对照：无新代次在途时旧行为不变）。
	if err := w.store.SaveResult(&CommandRecord{CommandID: "drop-3", LeaseEpoch: 9,
		Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 9, ExitCode: iptr(1),
		ResultText: "rejected", FinishedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	w.markStarted("drop-3", 9)
	w.Drop([]dropEntry{{CommandID: "drop-3", LeaseEpoch: 9}})
	if rec, _ := w.store.Load("drop-3", 9); rec != nil {
		t.Fatalf("same-generation dropped receipt must be deleted: %+v", rec)
	}
	if _, ok := w.generation("drop-3"); ok {
		t.Fatalf("same-generation drop must unmark started")
	}
}

// TestWorkerAckCleansThroughEpoch ACK 按「已确认代次」清理（R41-B1）：确认
// gen2 → gen1 残留与 gen2 结果一并出清；更高代次（gen3）的在途 intent 与执行权
// 不可触达；确认 gen3 后全部出清、在途标记摘除。
func TestWorkerAckCleansThroughEpoch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(true), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	const id = "ack-epoch"
	// 盘上三代并存：gen1 旧回执残留 + gen2 已上报回执 + gen3 在途 intent（新代
	// 次重执行中）。当前持有代次 = 3。
	w.markStarted(id, 3)
	for e, phase := range map[int64]func(*CommandRecord){
		1: func(r *CommandRecord) { r.ResultText, r.ExitCode, r.Phase = "gen1", iptr(0), PhaseResult },
		2: func(r *CommandRecord) { r.ResultText, r.ExitCode, r.Phase = "gen2", iptr(0), PhaseResult },
		3: func(r *CommandRecord) { r.Phase = PhaseIntent },
	} {
		rec := &CommandRecord{CommandID: id, LeaseEpoch: e, Kind: "ps_snapshot", TimeoutS: 5}
		phase(rec)
		if err := w.store.save(rec); err != nil {
			t.Fatal(err)
		}
	}
	// console 确认的是 gen2 的回执：清 ≤2，gen3 原样、执行权保留。
	w.Ack([]ackEntry{{CommandID: id, LeaseEpoch: 2}})
	for _, e := range []int64{1, 2} {
		if rec, _ := w.store.Load(id, e); rec != nil {
			t.Fatalf("gen%d must be cleaned by ack(2)", e)
		}
	}
	if rec, _ := w.store.Load(id, 3); rec == nil || rec.Phase != PhaseIntent {
		t.Fatalf("gen3 intent must survive ack(2): %+v", rec)
	}
	if gen, ok := w.generation(id); !ok || gen != 3 {
		t.Fatalf("gen3 execution right must survive ack(2): gen=%d ok=%v", gen, ok)
	}
	// gen3 回执落盘并被确认：全部出清 + 在途标记摘除。
	if err := w.store.SaveResult(&CommandRecord{CommandID: id, LeaseEpoch: 3,
		Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 300, ExitCode: iptr(0),
		ResultText: "gen3 done", FinishedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	w.Ack([]ackEntry{{CommandID: id, LeaseEpoch: 3}})
	if snaps := w.ResultsSnapshot(); len(snaps) != 0 {
		t.Fatalf("all generations must be cleaned after ack(3): %+v", snaps)
	}
	if _, ok := w.generation(id); ok {
		t.Fatalf("ack(3) must unmark the held generation")
	}
}

// waitResult 轮询等待某命令出结果（worker 异步执行）。
func waitResult(t *testing.T, w *CommandWorker, id string, check func(*CommandRecord)) *CommandRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range w.ResultsSnapshot() {
			if r.CommandID == id {
				if check != nil {
					check(r)
				}
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for result of %s", id)
	return nil
}

// TestWorkerSameEpochResendIdempotent 同代次重发幂等（R41-B1：重发同 epoch
// 不重执行）：gen1 在途执行（真实子进程未退出）期间 console 同代重发同一交付
// ——不入队、不取消、不重置，最终恰好一份回执；执行结束后补发的同代重发同样
// 去重（结果已在本地，回执持续重发直至 ACK）。
func TestWorkerSameEpochResendIdempotent(t *testing.T) {
	if _, err := os.Stat("/bin/ps"); err != nil {
		t.Skip("/bin/ps not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := testAgentState(t)
	w, err := NewCommandWorker(ctx, testAgentCfg(true), st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	d := delivery{CommandID: "idem-epoch", Kind: "ps_snapshot", TimeoutS: 5, ClaimedAt: 42, LeaseEpoch: 4}
	w.Deliver([]delivery{d}) // 首次交付：入队执行
	// 执行中同代重发 ×20（真实并发，与执行任意交错）。
	for i := 0; i < 20; i++ {
		w.Deliver([]delivery{d})
	}
	res := waitResult(t, w, "idem-epoch", func(r *CommandRecord) {
		if r.LeaseEpoch != 4 {
			t.Fatalf("receipt epoch = %d, want 4", r.LeaseEpoch)
		}
		if r.ExitCode == nil || *r.ExitCode != 0 {
			t.Fatalf("execution result: %+v", r)
		}
	})
	// 结果落盘后再重发：去重跳过，不产生新执行/新回执。
	w.Deliver([]delivery{d})
	time.Sleep(50 * time.Millisecond)
	snaps := w.ResultsSnapshot()
	if len(snaps) != 1 || snaps[0].LeaseEpoch != 4 || snaps[0].ResultText != res.ResultText {
		t.Fatalf("exactly one receipt must remain: %+v", snaps)
	}
	if gen, ok := w.generation("idem-epoch"); !ok || gen != 4 {
		t.Fatalf("held generation must stay 4: gen=%d ok=%v", gen, ok)
	}
	// 收尾：ACK 按（也是唯一）代次清理。
	w.Ack([]ackEntry{{CommandID: "idem-epoch", LeaseEpoch: 4}})
	if snaps := w.ResultsSnapshot(); len(snaps) != 0 {
		t.Fatalf("acked receipt must be removed: %+v", snaps)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
