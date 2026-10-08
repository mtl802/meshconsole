package store

// M1b-c 新增能力的 store 层单测（SPEC-M1b-c §2.2）：agent_tasks 心跳全量替换
// （进程消失 = 清行，不留历史）、节点删除级联、ai_agents 会话活跃度列往返。

import (
	"context"
	"testing"
	"time"
)

// TestAgentTasksFullReplace 心跳按节点全量替换：第二次上报后表内容 = 第二次
// 集合；空数组（present 且空）清空该节点任务（三态语义的「清空」半边）；另一
// 节点的任务不受影响。
func TestAgentTasksFullReplace(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id1 := seedNode(t, ctx, st, "cloud-1")
	id2 := seedNode(t, ctx, st, "mac-mini")

	first := []AgentTaskRow{
		{PID: 101, AgentName: "zcode", Cmd: "zcode serve", ElapsedS: 60,
			CPUPct: fptr(12.5), MemPct: fptr(1.5), StartedAt: sqlNull(time.Now().Unix() - 60)},
		{PID: 102, AgentName: "codex", Cmd: "codex review"},
	}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id1, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, &first, false); err != nil || !ok {
		t.Fatalf("heartbeat: ok=%v err=%v", ok, err)
	}
	// 节点 2 有自己的任务，随后验证隔离性。
	secondNode := []AgentTaskRow{{PID: 201, AgentName: "claude", Cmd: "claude --resume"}}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id2, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, &secondNode, false); err != nil || !ok {
		t.Fatalf("heartbeat n2: ok=%v err=%v", ok, err)
	}

	// 第二次上报：进程 101 消失（只换新集合，无历史行）、102 换 pid。
	next := []AgentTaskRow{{PID: 103, AgentName: "zcode", Cmd: "zcode chat", ElapsedS: 5}}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id1, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, &next, false); err != nil || !ok {
		t.Fatalf("heartbeat 2: ok=%v err=%v", ok, err)
	}
	rows, err := st.ListAgentTasks(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PID != 103 || rows[0].AgentName != "zcode" || rows[0].Cmd != "zcode chat" {
		t.Fatalf("rows = %+v, want exactly second set (无历史行)", rows)
	}
	// 隔离性：节点 2 的任务原样。
	rows2, err := st.ListAgentTasks(ctx, id2)
	if err != nil || len(rows2) != 1 || rows2[0].AgentName != "claude" {
		t.Fatalf("rows2 = %+v err=%v", rows2, err)
	}

	// 空数组 = 清空该节点任务（进程全消失的合法表达）。
	empty := []AgentTaskRow{}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id1, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, &empty, false); err != nil || !ok {
		t.Fatalf("heartbeat empty: ok=%v err=%v", ok, err)
	}
	rows, _ = st.ListAgentTasks(ctx, id1)
	if len(rows) != 0 {
		t.Fatalf("empty array must clear node tasks, got %d rows", len(rows))
	}

	// 缺席（nil）= 无变化：节点 2 任务仍在。
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id2, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, nil, false); err != nil || !ok {
		t.Fatalf("heartbeat absent: ok=%v err=%v", ok, err)
	}
	rows2, _ = st.ListAgentTasks(ctx, id2)
	if len(rows2) != 1 {
		t.Fatalf("absent field must not touch tasks, got %d rows", len(rows2))
	}
}

// TestAgentTasksNullDimensions 可空维度（Windows 兜底）如实存 NULL：
// started_at/cpu/mem 缺失 → 读回 nil；node 删除级联清任务行。
func TestAgentTasksNullDimensions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedNode(t, ctx, st, "cloud-1")
	tasks := []AgentTaskRow{{PID: 7, AgentName: "codex"}}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, &tasks, false); err != nil || !ok {
		t.Fatalf("heartbeat: ok=%v err=%v", ok, err)
	}
	rows, err := st.ListAgentTasks(ctx, id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].CPUPct.Valid || rows[0].MemPct.Valid || rows[0].StartedAt.Valid {
		t.Fatalf("null dimensions must stay NULL: %+v", rows[0])
	}
	// 全网读路径。
	all, err := st.ListAllAgentTasks(ctx)
	if err != nil || len(all) != 1 || all[0].NodeID != id {
		t.Fatalf("all = %+v err=%v", all, err)
	}
	// 节点删除 → 任务行级联清空（外键 CASCADE）。
	if _, err := st.DeleteNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	all, _ = st.ListAllAgentTasks(ctx)
	if len(all) != 0 {
		t.Fatalf("node delete must cascade agent_tasks, got %d rows", len(all))
	}
}

// TestAgentsLastActivityRoundTrip ai_agents 新列（last_activity/session_files）
// 心跳写入 → 读回；下一拍缺席（nil）时覆盖为 NULL（以本次上报为准）。
func TestAgentsLastActivityRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedNode(t, ctx, st, "cloud-1")
	act, files := int64(1_800_000_000), int64(412)
	ags := []AgentRow{{Name: "zcode", Type: "cli", Status: "active",
		LastActivity: &act, SessionFiles: &files}}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, &ags, nil, false); err != nil || !ok {
		t.Fatalf("heartbeat: ok=%v err=%v", ok, err)
	}
	recs, err := st.ListAgents(ctx, id)
	if err != nil || len(recs) != 1 {
		t.Fatalf("agents=%v err=%v", recs, err)
	}
	if recs[0].LastActivity == nil || *recs[0].LastActivity != act {
		t.Fatalf("last_activity = %v, want %d", recs[0].LastActivity, act)
	}
	if recs[0].SessionFiles == nil || *recs[0].SessionFiles != 412 {
		t.Fatalf("session_files = %v, want 412", recs[0].SessionFiles)
	}
	// 下一拍 agent 侧没拿到活跃度（nil）→ 覆盖为 NULL（不沿用旧值）。
	ags2 := []AgentRow{{Name: "zcode", Type: "cli", Status: "active"}}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, &ags2, nil, false); err != nil || !ok {
		t.Fatal(err)
	}
	recs, _ = st.ListAgents(ctx, id)
	if recs[0].LastActivity != nil || recs[0].SessionFiles != nil {
		t.Fatalf("absent activity must overwrite to NULL: %+v", recs[0])
	}
}

// TestMigrationV4Schema v4 落库断言：agent_tasks 表存在且约束齐全；
// ai_agents 带新列（直接 SQL 探测 schema_migrations 版本 + 列存在性）。
func TestMigrationV4Schema(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	var version int
	if err := st.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 4 {
		t.Fatalf("schema version = %d, want ≥4", version)
	}
	// ai_agents 新列存在性（PRAGMA table_info）。
	rows, err := st.read.QueryContext(ctx, `PRAGMA table_info(ai_agents)`)
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	rows.Close()
	for _, want := range []string{"last_activity", "session_files"} {
		if !cols[want] {
			t.Fatalf("ai_agents missing column %q (migration v4)", want)
		}
	}
}

// TestMigrationV5Schema v5 落库断言（R27-#4）：nodes.tasks_truncated 列存在，
// 旧行默认 false。
func TestMigrationV5Schema(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	var version int
	if err := st.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 5 {
		t.Fatalf("schema version = %d, want ≥5", version)
	}
	rows, err := st.read.QueryContext(ctx, `PRAGMA table_info(nodes)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "tasks_truncated" {
			found = true
		}
	}
	if !found {
		t.Fatal("nodes missing column tasks_truncated (migration v5)")
	}
}

// TestHeartbeatTasksTruncatedFlag 任务清单截断标记（R27-#4）：随显式快照写入
// （true/false 皆然），快照缺席不改动；读路径 GetNodeByName 带出。
func TestHeartbeatTasksTruncatedFlag(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedNode(t, ctx, st, "cloud-1")

	if n, err := st.GetNodeByName(ctx, "cloud-1"); err != nil || n.TasksTruncated {
		t.Fatalf("fresh node tasks_truncated = %v err=%v, want false", n.TasksTruncated, err)
	}

	tasks := []AgentTaskRow{{PID: 1, AgentName: "zcode"}}
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, &tasks, true); err != nil || !ok {
		t.Fatalf("heartbeat truncated: ok=%v err=%v", ok, err)
	}
	if n, err := st.GetNodeByName(ctx, "cloud-1"); err != nil || !n.TasksTruncated {
		t.Fatalf("truncated snapshot must set flag: %v err=%v", n.TasksTruncated, err)
	}

	// 快照缺席：标记不改动（仍描述上一次快照）。
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, nil, false); err != nil || !ok {
		t.Fatal(err)
	}
	if n, err := st.GetNodeByName(ctx, "cloud-1"); err != nil || !n.TasksTruncated {
		t.Fatalf("absent snapshot must not touch flag: %v err=%v", n.TasksTruncated, err)
	}

	// 未截断的显式快照：标记复位。
	if ok, err := st.HeartbeatFull(ctx, &MetricsRow{NodeID: id, TS: time.Now().Unix(), CPUPct: fptr(1)}, "v", nil, nil, &tasks, false); err != nil || !ok {
		t.Fatal(err)
	}
	if n, err := st.GetNodeByName(ctx, "cloud-1"); err != nil || n.TasksTruncated {
		t.Fatalf("untruncated snapshot must reset flag: %v err=%v", n.TasksTruncated, err)
	}
}
