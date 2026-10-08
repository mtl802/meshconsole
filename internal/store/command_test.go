package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---- M1d（SPEC-M1d §1/§2）命令通道存储层测试 ----
// 覆盖：状态机全迁移与异常路径（lease 过期重发 / unknown / 迟到回执修正与拒绝 /
// 双实例 claim 竞态）、幂等键（同键同参/异参/窗口过期归档）、配额、l2 领取复核、
// 归档与 90 天清理。

// seedL2Node 建节点并打开 l2 白名单（提交/领取测试前置）。
func seedL2Node(t *testing.T, ctx context.Context, st *Store, name string) int64 {
	t.Helper()
	id := seedNode(t, ctx, st, name)
	if _, err := st.write.Exec(`UPDATE nodes SET l2_allowed = 1, caps = '["linux-systemd","linux-docker"]', status='online' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

var testCmdSeq int64

func submitTestCmd(t *testing.T, ctx context.Context, st *Store, nodeID int64, key string, now int64) *CommandRecord {
	t.Helper()
	testCmdSeq++
	row, _, err := st.SubmitCommand(ctx, &CommandRow{
		CommandID: fmt.Sprintf("cmd-%s-%d", key, testCmdSeq), NodeID: nodeID,
		Kind: "ps_snapshot", ArgsJSON: "{}", Status: CommandPending, TimeoutS: 30,
		SubmittedKey: sql.NullString{String: key, Valid: key != ""}, CreatedBy: "tester",
		Scope: "operator", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("submit %s: %v", key, err)
	}
	return row
}

func fmtInt(v int64) string { return fmt.Sprintf("%d", v) }

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// TestCommandStateTransitions 状态机主干：pending → claimed → running → 终态，
// 回执归属验证与 exit code 判定。
func TestCommandStateTransitions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()

	row := submitTestCmd(t, ctx, st, id, "k1", now)
	if row.Status != CommandPending {
		t.Fatalf("status = %s, want pending", row.Status)
	}

	// 领取：原子置 claimed + claimed_at + lease_epoch（首次领取=1，R41-B1）。
	claimed, err := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, CommandClaimLimit)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: n=%d err=%v", len(claimed), err)
	}
	if claimed[0].Status != CommandClaimed || !claimed[0].ClaimedAt.Valid {
		t.Fatalf("claimed status=%s claimed_at=%v", claimed[0].Status, claimed[0].ClaimedAt)
	}
	if claimed[0].LeaseEpoch != 1 {
		t.Fatalf("first claim lease_epoch = %d, want 1", claimed[0].LeaseEpoch)
	}
	epoch := claimed[0].LeaseEpoch

	// 租约内重复领取：不得重复下发（双实例 claim 竞态的第一层防护）。
	again, err := st.ClaimNodeCommands(ctx, id, now+1, CommandLeaseS, CommandClaimLimit)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-claim within lease must be empty: n=%d err=%v", len(again), err)
	}

	// running 报告：claimed → running（timeout×1.5 unknown 判定起点）。
	if n, err := st.MarkCommandsRunning(ctx, id, []string{row.CommandID}, now+2); err != nil || n != 1 {
		t.Fatalf("mark running: n=%d err=%v", n, err)
	}

	// 回执（lease_epoch 匹配）→ succeeded。
	zero := int64(0)
	out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: row.CommandID, LeaseEpoch: epoch, ExitCode: nullInt(zero),
		ResultText: "ok", RecordedAt: now + 3,
	}})
	if err != nil || len(out) != 1 || !out[0].Ack || out[0].Status != CommandSucceeded {
		t.Fatalf("result: %+v err=%v", out, err)
	}
	got, _ := st.GetCommand(ctx, row.CommandID)
	if got.Status != CommandSucceeded || got.ExitCode.Int64 != 0 || got.FinishedAt.Int64 != now+3 {
		t.Fatalf("terminal row: %+v", got)
	}
	// 已记录结果 → ACK 名单包含（条目携带回执代次，agent 据此清理本地副本）。
	acks, err := st.AckableCommandIDs(ctx, id, []string{row.CommandID})
	if err != nil || len(acks) != 1 {
		t.Fatalf("ackable: %v err=%v", acks, err)
	}
	if acks[0].LeaseEpoch != 1 || acks[0].CommandID != row.CommandID {
		t.Fatalf("ackable entry: %+v", acks[0])
	}
}

func nullInt(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: true}
}

// TestLeaseExpiryResend lease 过期重发（R41-B1 代次令牌）：租约外 re-claim 产生
// 新 claimed_at 且 lease_epoch 自增（1→2），旧代次迟到回执被拒（drop "stale
// epoch"，内容不触达命令状态），新代次回执被收——重发至终态且不存在旧实例
// 覆盖新结果的路径。
func TestLeaseExpiryResend(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()

	row := submitTestCmd(t, ctx, st, id, "k2", now)
	first, err := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, 3)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %v", err)
	}
	lease1, epoch1 := first[0].ClaimedAt.Int64, first[0].LeaseEpoch

	// 租约过期（now+CommandLeaseS+1）→ 可重领，claimed_at 前移、代次自增。
	second, err := st.ClaimNodeCommands(ctx, id, now+CommandLeaseS+1, CommandLeaseS, 3)
	if err != nil || len(second) != 1 {
		t.Fatalf("re-claim: n=%d err=%v", len(second), err)
	}
	lease2, epoch2 := second[0].ClaimedAt.Int64, second[0].LeaseEpoch
	if lease2 <= lease1 {
		t.Fatalf("re-claim must advance claimed_at: %d -> %d", lease1, lease2)
	}
	if epoch1 != 1 || epoch2 != 2 {
		t.Fatalf("lease_epoch must increment on reclaim: %d -> %d", epoch1, epoch2)
	}

	// 旧代次迟到回执（epoch1）→ 拒绝且拒因可辨（stale epoch），状态不动；
	// 结果条目携带回执自陈代次（agent drop 清理目标）。
	out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: row.CommandID, ClaimedAt: lease1, LeaseEpoch: epoch1,
		ExitCode: nullInt(0), ResultText: "stale",
		RecordedAt: now + CommandLeaseS + 2,
	}})
	if err != nil || !out[0].Drop || out[0].DropWhy != "stale epoch" {
		t.Fatalf("stale epoch result must drop as stale: %+v err=%v", out, err)
	}
	if out[0].LeaseEpoch != epoch1 {
		t.Fatalf("drop outcome must carry the rejected receipt's epoch (agent cleanup target): %+v", out[0])
	}
	if got, _ := st.GetCommand(ctx, row.CommandID); got.Status != CommandClaimed || got.ResultText != "" {
		t.Fatalf("stale receipt must not touch the row: %+v", got)
	}
	// 新代次回执（epoch2）→ 接受。
	out, err = st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: row.CommandID, ClaimedAt: lease2, LeaseEpoch: epoch2,
		ExitCode: nullInt(0), ResultText: "fresh",
		RecordedAt: now + CommandLeaseS + 3,
	}})
	if err != nil || !out[0].Ack || out[0].Status != CommandSucceeded {
		t.Fatalf("fresh epoch result: %+v err=%v", out, err)
	}
}

// TestUnknownSweepAndLateCorrection unknown 语义：lease×2 / running 后
// timeout×1.5 无回执 → unknown+degraded（非 failed）；迟到回执内容匹配
// （command_id + claimed_at）→ 修正为实际终态并保留 degraded 历史。
func TestUnknownSweepAndLateCorrection(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()

	// 命令 A：claimed 后失联 → lease×2 → unknown。
	a := submitTestCmd(t, ctx, st, id, "ka", now)
	ca, _ := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, 3)
	epoch := ca[0].LeaseEpoch
	// 命令 B：claimed（+5s 领取，与 A 的租约起点错开供迟到回执不匹配用例）
	// → running（timeout 30s）→ 1.5×30=45s 无回执 → unknown。
	b := submitTestCmd(t, ctx, st, id, "kb", now)
	cb, _ := st.ClaimNodeCommands(ctx, id, now+5, CommandLeaseS, 3)
	if len(cb) != 1 {
		t.Fatalf("claim b: %d", len(cb))
	}
	if _, err := st.MarkCommandsRunning(ctx, id, []string{b.CommandID}, now+1); err != nil {
		t.Fatal(err)
	}

	// 未到阈值：不迁移。
	if n, _ := st.SweepUnknown(ctx, now+10, CommandLeaseS); n != 0 {
		t.Fatalf("premature unknown sweep: %d", n)
	}
	// 阈值后：B 在 running_at(+6s)+45s 触发（running timeout×1.5），A 要到 lease×2。
	if n, _ := st.SweepUnknown(ctx, now+6+45, CommandLeaseS); n != 1 {
		t.Fatalf("running timeout×1.5 sweep: %d", n)
	}
	if n, _ := st.SweepUnknown(ctx, now+2*CommandLeaseS+1, CommandLeaseS); n != 1 {
		t.Fatalf("lease×2 sweep: %d", n)
	}
	gotA, _ := st.GetCommand(ctx, a.CommandID)
	gotB, _ := st.GetCommand(ctx, b.CommandID)
	for name, got := range map[string]*CommandRecord{"A": gotA, "B": gotB} {
		if got.Status != CommandUnknown || !got.Degraded {
			t.Fatalf("cmd %s: status=%s degraded=%v, want unknown+degraded", name, got.Status, got.Degraded)
		}
	}

	// unknown 不可领取（永不自动重发，DESIGN §4.1-D）。
	re, err := st.ClaimNodeCommands(ctx, id, now+2*CommandLeaseS+2, CommandLeaseS, 3)
	if err != nil || len(re) != 0 {
		t.Fatalf("unknown must not be claimable: %d", len(re))
	}

	// 迟到回执（当前代次 lease_epoch 匹配）→ 修正为实际终态，degraded 保留。
	out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: a.CommandID, LeaseEpoch: epoch, ExitCode: nullInt(0), ResultText: "late but real",
		RecordedAt: now + 3*CommandLeaseS,
	}})
	if err != nil || !out[0].Ack || out[0].Status != CommandSucceeded {
		t.Fatalf("late correction: %+v err=%v", out, err)
	}
	gotA, _ = st.GetCommand(ctx, a.CommandID)
	if gotA.Status != CommandSucceeded || !gotA.Degraded {
		t.Fatalf("corrected row: status=%s degraded=%v, want succeeded + degraded history", gotA.Status, gotA.Degraded)
	}

	// unknown 上旧代次/未持有代次的迟到回执（epoch=2，B 从未被重领过）→ 拒绝，
	// 维持 unknown。
	out, err = st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: b.CommandID, LeaseEpoch: epoch + 1, ExitCode: nullInt(1), ResultText: "mismatch",
		RecordedAt: now + 3*CommandLeaseS,
	}})
	if err != nil || !out[0].Drop {
		t.Fatalf("mismatched late result must drop: %+v", out)
	}
	if got, _ := st.GetCommand(ctx, b.CommandID); got.Status != CommandUnknown {
		t.Fatalf("b must stay unknown, got %s", got.Status)
	}
}

// TestInterruptedReceiptAlignsUnknown agent 重启 intent 无结果 → interrupted
// 回执 → unknown+degraded 对齐并 ACK（agent 删本地副本）。
func TestInterruptedReceiptAlignsUnknown(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()
	row := submitTestCmd(t, ctx, st, id, "ki", now)
	ca, _ := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, 3)
	out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: row.CommandID, LeaseEpoch: ca[0].LeaseEpoch,
		ResultText: "agent 重启：执行中断", Interrupted: true, RecordedAt: now + 5,
	}})
	if err != nil || !out[0].Ack || out[0].Status != CommandUnknown {
		t.Fatalf("interrupted: %+v err=%v", out, err)
	}
	got, _ := st.GetCommand(ctx, row.CommandID)
	if got.Status != CommandUnknown || !got.Degraded || !got.ResultRecorded {
		t.Fatalf("aligned row: %+v", got)
	}
	// result_recorded=1 → ACK 名单包含。
	if acks, _ := st.AckableCommandIDs(ctx, id, []string{row.CommandID}); len(acks) != 1 {
		t.Fatalf("interrupted receipt must be ackable")
	}
}

// TestSubmissionIdempotency 幂等键：同键同参窗口内返回原命令；同键异参冲突；
// 窗口过期归档（archived_key 只归档提交键，command_id 永不复用）后键可复用。
func TestSubmissionIdempotency(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()

	r1 := submitTestCmd(t, ctx, st, id, "idem-1", now)
	// 同键同参 → 幂等命中（同一 command_id，不新建）。
	dup, idem, err := st.SubmitCommand(ctx, &CommandRow{
		CommandID: "cmd-should-not-exist", NodeID: id, Kind: "ps_snapshot",
		ArgsJSON: "{}", Status: CommandPending, TimeoutS: 30,
		SubmittedKey: nullString("idem-1"), CreatedBy: "tester", Scope: "operator",
		CreatedAt: now + 1, UpdatedAt: now + 1,
	})
	if err != nil || !idem || dup.CommandID != r1.CommandID {
		t.Fatalf("idempotent resubmit: dup=%s idem=%v err=%v", dup.CommandID, idem, err)
	}
	// 同键异参 → 冲突。
	if _, _, err := st.SubmitCommand(ctx, &CommandRow{
		CommandID: "cmd-other-args", NodeID: id, Kind: "df_report", ArgsJSON: "{}",
		Status: CommandPending, TimeoutS: 30, SubmittedKey: nullString("idem-1"),
		CreatedAt: now + 2, UpdatedAt: now + 2,
	}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("same key different args: err=%v, want ErrKeyConflict", err)
	}
	// 窗口过期 → 归档旧键后同名键可复用，旧行 command_id 不变。
	ok, err := st.ArchiveSubmissionKey(ctx, "idem-1", now+601)
	if err != nil || !ok {
		t.Fatalf("archive key: %v %v", ok, err)
	}
	old, _ := st.GetCommand(ctx, r1.CommandID)
	if old.ArchivedKey.String != "idem-1" || old.SubmittedKey.Valid {
		t.Fatalf("archived row: key=%v archived_key=%v", old.SubmittedKey, old.ArchivedKey)
	}
	r2, idem, err := st.SubmitCommand(ctx, &CommandRow{
		CommandID: "cmd-reused-key", NodeID: id, Kind: "ps_snapshot", ArgsJSON: "{}",
		Status: CommandPending, TimeoutS: 30, SubmittedKey: nullString("idem-1"),
		CreatedBy: "tester", Scope: "operator", CreatedAt: now + 602, UpdatedAt: now + 602,
	})
	if err != nil || idem || r2.CommandID == r1.CommandID {
		t.Fatalf("key reuse after archive: %s idem=%v err=%v", r2.CommandID, idem, err)
	}
}

// TestCommandQuota 提交配额：每节点 pending+claimed+running ≤5（事务内计数），
// 超出 429（ErrQuotaExceeded）；终态/归档不计。
func TestCommandQuota(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()
	for i := 0; i < CommandQuotaPerNode; i++ {
		submitTestCmd(t, ctx, st, id, "q"+fmtInt(int64(i)), now)
	}
	if _, _, err := st.SubmitCommand(ctx, &CommandRow{
		CommandID: "cmd-over-quota", NodeID: id, Kind: "ps_snapshot", ArgsJSON: "{}",
		Status: CommandPending, TimeoutS: 30, CreatedAt: now, UpdatedAt: now,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota: err=%v, want ErrQuotaExceeded", err)
	}
	// 一条进终态后腾出名额。
	c, _ := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, 3)
	out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: c[0].CommandID, LeaseEpoch: c[0].LeaseEpoch, ExitCode: nullInt(0),
		RecordedAt: now + 1,
	}})
	if err != nil || !out[0].Ack {
		t.Fatalf("finish one: %+v err=%v", out, err)
	}
	if _, _, err := st.SubmitCommand(ctx, &CommandRow{
		CommandID: "cmd-after-slot", NodeID: id, Kind: "ps_snapshot", ArgsJSON: "{}",
		Status: CommandPending, TimeoutS: 30, CreatedAt: now + 2, UpdatedAt: now + 2,
	}); err != nil {
		t.Fatalf("submit after terminal slot freed: %v", err)
	}
}

// TestClaimL2Gate 领取路径 l2 双重复核②：白名单外节点（云）不返回任何命令，
// 即便提交侧被绕过（防御纵深）。
func TestClaimL2Gate(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cloud := seedNode(t, ctx, st, "cloud-nanjing") // l2_allowed=0
	now := time.Now().Unix()
	// 直接落库（模拟提交侧被绕过的最坏情形）。
	if _, _, err := st.SubmitCommand(ctx, &CommandRow{
		CommandID: "cmd-cloud", NodeID: cloud, Kind: "ps_snapshot", ArgsJSON: "{}",
		Status: CommandPending, TimeoutS: 30, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ClaimNodeCommands(ctx, cloud, now+1, CommandLeaseS, 3)
	if err != nil || len(got) != 0 {
		t.Fatalf("cloud claim must be empty: n=%d err=%v", len(got), err)
	}
}

// TestResultOversizeRejected 回执 64KB 硬上限：超限拒绝（drop），不落库。
func TestResultOversizeRejected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()
	row := submitTestCmd(t, ctx, st, id, "big", now)
	ca, _ := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, 3)
	out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: row.CommandID, LeaseEpoch: ca[0].LeaseEpoch,
		ResultText: strings.Repeat("x", CommandResultsMaxBytes+1), RecordedAt: now,
	}})
	if err != nil || !out[0].Drop {
		t.Fatalf("oversize result must drop: %+v err=%v", out, err)
	}
}

// TestCrossNodeReceiptRejected 结果归属验证：A 节点回执 B 节点命令 → 拒绝。
func TestCrossNodeReceiptRejected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	a := seedL2Node(t, ctx, st, "mac-mini")
	b := seedL2Node(t, ctx, st, "windows")
	now := time.Now().Unix()
	row := submitTestCmd(t, ctx, st, a, "x", now)
	ca, _ := st.ClaimNodeCommands(ctx, a, now, CommandLeaseS, 3)
	out, err := st.RecordCommandResults(ctx, b, []ResultIn{{
		CommandID: row.CommandID, LeaseEpoch: ca[0].LeaseEpoch, ExitCode: nullInt(0),
		RecordedAt: now + 1,
	}})
	if err != nil || !out[0].Drop {
		t.Fatalf("cross-node receipt must drop: %+v err=%v", out, err)
	}
}

// TestStaleEpochReceiptRules 回执代次校验的边界规则（R41-B1）：未领取命令的
// 回执（epoch=0/1 均与库值 0 不匹配）→ stale epoch drop；已记录回执后同代次
// 重复回执 → 幂等 ACK 不覆盖；已记录后异代次回执 → drop。
func TestStaleEpochReceiptRules(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedL2Node(t, ctx, st, "mac-mini")
	now := time.Now().Unix()

	// 未领取（epoch=0）：任何代次声明的回执都无归属。
	row := submitTestCmd(t, ctx, st, id, "se0", now)
	for _, e := range []int64{0, 1} {
		out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
			CommandID: row.CommandID, LeaseEpoch: e, ExitCode: nullInt(0),
			ResultText: "premature", RecordedAt: now,
		}})
		if err != nil || !out[0].Drop || out[0].DropWhy != "stale epoch" {
			t.Fatalf("epoch %d receipt on pending row must drop stale: %+v err=%v", e, out, err)
		}
	}
	if got, _ := st.GetCommand(ctx, row.CommandID); got.Status != CommandPending {
		t.Fatalf("pending row must stay pending, got %s", got.Status)
	}

	// 已记录回执后：同代次重复 → 幂等 ACK 不覆盖结果；异代次 → drop。
	c, _ := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, 3)
	out, err := st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: c[0].CommandID, LeaseEpoch: c[0].LeaseEpoch, ExitCode: nullInt(0),
		ResultText: "genuine", RecordedAt: now + 1,
	}})
	if err != nil || !out[0].Ack {
		t.Fatalf("record: %+v err=%v", out, err)
	}
	out, err = st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: c[0].CommandID, LeaseEpoch: c[0].LeaseEpoch, ExitCode: nullInt(1),
		ResultText: "DUPLICATED", RecordedAt: now + 2,
	}})
	if err != nil || !out[0].Ack || out[0].Status != CommandSucceeded {
		t.Fatalf("same-epoch duplicate must idempotently re-ack: %+v err=%v", out, err)
	}
	if got, _ := st.GetCommand(ctx, c[0].CommandID); got.ResultText != "genuine" {
		t.Fatalf("duplicate must not overwrite result: %q", got.ResultText)
	}
	out, err = st.RecordCommandResults(ctx, id, []ResultIn{{
		CommandID: c[0].CommandID, LeaseEpoch: c[0].LeaseEpoch + 1, ExitCode: nullInt(1),
		ResultText: "forged generation", RecordedAt: now + 3,
	}})
	if err != nil || !out[0].Drop || out[0].DropWhy != "stale epoch" {
		t.Fatalf("future-epoch receipt must drop stale: %+v err=%v", out, err)
	}
}

// TestArchiveStalePendingAndRetention 离线 pending 24h → archived（不可再领取、
// 不占配额）；90 天保留清理只删过期行。
func TestArchiveStalePendingAndRetention(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedNode(t, ctx, st, "offline-node") // 无心跳 → SweepOffline 后非 online
	if _, err := st.write.Exec(`UPDATE nodes SET status='offline' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	row := submitTestCmd(t, ctx, st, id, "stale", now-25*3600)
	if n, err := st.ArchiveStalePending(ctx, now, now-24*3600); err != nil || n != 1 {
		t.Fatalf("archive: n=%d err=%v", n, err)
	}
	got, _ := st.GetCommand(ctx, row.CommandID)
	if !got.Archived || got.Status != CommandPending {
		t.Fatalf("archived row: archived=%v status=%s", got.Archived, got.Status)
	}
	// archived 不可领取。
	if c, _ := st.ClaimNodeCommands(ctx, id, now, CommandLeaseS, 3); len(c) != 0 {
		t.Fatalf("archived must not be claimable")
	}
	// 90 天清理：老行删除、新行保留。
	if _, err := st.write.Exec(`UPDATE commands SET created_at = ? WHERE command_id = ?`, now-91*24*3600, row.CommandID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CleanupCommands(ctx, now-90*24*3600); err != nil || n != 1 {
		t.Fatalf("cleanup: n=%d err=%v", n, err)
	}
	if got, _ := st.GetCommand(ctx, row.CommandID); got != nil {
		t.Fatalf("row must be deleted after retention")
	}
}
