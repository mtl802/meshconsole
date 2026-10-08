package meshview_test

// M1b-c 视图层单测（SPEC-M1b-c §2.2）：overview 聚合扩展（running_tasks +
// agents.last_activity）与 AgentTasks 节点过滤。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/meshview"
	"github.com/mtl802/meshconsole/internal/store"
)

// seedTasks 在 seed 之上给 cloud-1 布一条运行任务快照 + agent 活跃度。
func seedTasks(t *testing.T) *store.Store {
	t.Helper()
	st, _ := seed(t)
	ctx := context.Background()
	n, err := st.GetNodeByName(ctx, "cloud-1")
	if err != nil || n == nil {
		t.Fatalf("seed node missing: %v", err)
	}
	act := time.Now().Add(-2 * time.Minute).Unix()
	files := int64(412)
	cpu := 12.5
	started := time.Now().Add(-5 * time.Minute).Unix()
	_, err = st.HeartbeatFull(ctx, &store.MetricsRow{
		NodeID: n.ID, TS: time.Now().Unix(), CPUPct: &cpu,
	}, "agent-1",
		nil,
		&[]store.AgentRow{{Name: "zcode", Type: "cli", Status: "active",
			LastActivity: &act, SessionFiles: &files}},
		&[]store.AgentTaskRow{{PID: 4242, AgentName: "zcode", Cmd: "zcode serve",
			ElapsedS: 300, CPUPct: &cpu, StartedAt: sqlStart(started)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func sqlStart(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: true}
}

// TestOverviewRunningTasksAndActivity overview 聚合带 running_tasks 与
// agents.last_activity/session_files（SPEC-M1b-c §2.2）。
func TestOverviewRunningTasksAndActivity(t *testing.T) {
	st := seedTasks(t)
	q := meshview.New(st)
	ov, err := q.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.RunningTasks) != 1 {
		t.Fatalf("running_tasks = %+v, want 1", ov.RunningTasks)
	}
	task := ov.RunningTasks[0]
	if task.Node != "cloud-1" || task.AgentName != "zcode" || task.PID != 4242 || task.ElapsedS != 300 {
		t.Fatalf("task = %+v", task)
	}
	if task.CPUPct == nil || *task.CPUPct != 12.5 {
		t.Fatalf("task cpu = %v", task.CPUPct)
	}
	if task.StartedAt == nil {
		t.Fatal("started_at must be exposed when known")
	}
	// agents 带 last_activity。
	var found bool
	for _, a := range ov.Agents {
		if a.Name == "zcode" {
			found = true
			if a.LastActivity == nil || a.SessionFiles == nil || *a.SessionFiles != 412 {
				t.Fatalf("agent activity = %+v/%+v", a.LastActivity, a.SessionFiles)
			}
		}
	}
	if !found {
		t.Fatal("zcode missing from overview agents")
	}
}

// TestAgentTasksNodeFilter AgentTasks 按节点过滤；未知节点 → 空清单（工具语义）。
func TestAgentTasksNodeFilter(t *testing.T) {
	st := seedTasks(t)
	q := meshview.New(st)
	ctx := context.Background()

	tasks, err := q.AgentTasks(ctx, "cloud-1")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("cloud-1 tasks = %v err=%v", tasks, err)
	}
	tasks, err = q.AgentTasks(ctx, "mac-mini")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("mac-mini tasks = %v err=%v (无任务节点应为空清单)", tasks, err)
	}
	tasks, err = q.AgentTasks(ctx, "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("all-node tasks = %v err=%v", tasks, err)
	}
}

// TestAgentTasksSnapshotConsistency 任务清单与截断标记同一读事务（R31-#2 →
// R33-#7）：AgentTasksSnapshot 一次取得 tasks 与 tasks_truncated 并集，口径
// 与 MCP 消费一致——按节点过滤取该节点标记、未知节点 false、被截断节点零任务
// 时标记仍为 true（并集按节点计，不因空快照翻转）。
func TestAgentTasksSnapshotConsistency(t *testing.T) {
	st, path := seed(t)
	q := meshview.New(st)
	ctx := context.Background()
	n, err := st.GetNodeByName(ctx, "cloud-1")
	if err != nil || n == nil {
		t.Fatalf("seed node missing: %v", err)
	}
	cpu := 42.0
	if _, err := st.HeartbeatFull(ctx, &store.MetricsRow{
		NodeID: n.ID, TS: time.Now().Unix(), CPUPct: &cpu,
	}, "agent-1", nil, nil,
		&[]store.AgentTaskRow{{PID: 7, AgentName: "zcode", ElapsedS: 10, CPUPct: &cpu}}, true); err != nil {
		t.Fatal(err)
	}

	// 全网：任务 + 截断标记同批返回。
	tasks, truncated, err := q.AgentTasksSnapshot(ctx, "")
	if err != nil || len(tasks) != 1 || !truncated {
		t.Fatalf("all = (%d tasks, trunc=%v) err=%v, want 1+true", len(tasks), truncated, err)
	}
	// 按节点过滤 = 该节点自己的标记。
	tasks, truncated, err = q.AgentTasksSnapshot(ctx, "cloud-1")
	if err != nil || len(tasks) != 1 || !truncated {
		t.Fatalf("cloud-1 = (%d tasks, trunc=%v) err=%v, want 1+true", len(tasks), truncated, err)
	}
	// 未知节点：空清单 + false。
	_, truncated, err = q.AgentTasksSnapshot(ctx, "ghost")
	if err != nil || truncated {
		t.Fatalf("ghost trunc=%v err=%v, want false", truncated, err)
	}
	// 被截断标记的节点当前零任务：标记保持 true（按节点计的并集语义）。
	rawExec(t, path, `UPDATE nodes SET tasks_truncated = 1 WHERE name = 'mac-mini'`)
	_, truncated, err = q.AgentTasksSnapshot(ctx, "mac-mini")
	if err != nil || !truncated {
		t.Fatalf("mac-mini zero-task trunc=%v err=%v, want true", truncated, err)
	}
	// AgentTasks（overview 用薄封装）行为不回退。
	tasks, err = q.AgentTasks(ctx, "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("AgentTasks wrapper = %v err=%v", tasks, err)
	}
}

// rawExec 测试直连库执行写语句（视图层只读，构造数据态走原始 SQL，backdateService 同口径）。
func rawExec(t *testing.T, path, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// TestAgentTaskStaleMarking 过期快照标记（R27-#3）：离线节点的任务即使快照
// 时间戳新鲜也 stale（不冒充运行中）；在线节点的新鲜快照不 stale；在线节点
// 快照超 taskStaleAfter 未刷新（进程扫描持续失败时 agent_tasks 缺席、旧行保留
// 的库内形态）→ stale。JSON 契约带 stale 键。
func TestAgentTaskStaleMarking(t *testing.T) {
	st, path := seed(t)
	q := meshview.New(st)
	ctx := context.Background()

	n1, err := st.GetNodeByName(ctx, "cloud-1")
	if err != nil || n1 == nil {
		t.Fatal(err)
	}
	cpu := 12.5
	tasks := []store.AgentTaskRow{{PID: 101, AgentName: "zcode", Cmd: "zcode serve", CPUPct: &cpu}}
	if _, err := st.HeartbeatFull(ctx, &store.MetricsRow{NodeID: n1.ID, TS: time.Now().Unix(), CPUPct: &cpu},
		"agent-1", nil, nil, &tasks, false); err != nil {
		t.Fatal(err)
	}
	// mac-mini（offline）一条时间戳新鲜的遗留任务：节点失联即 stale。
	n2, err := st.GetNodeByName(ctx, "mac-mini")
	if err != nil || n2 == nil {
		t.Fatal(err)
	}
	rawExec(t, path, `INSERT INTO agent_tasks(node_id, pid, agent_name, cmd, elapsed_s, updated_at)
		VALUES(?, 202, 'codex', 'codex review', 30, ?)`, n2.ID, time.Now().Unix())

	all, err := q.AgentTasks(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	byNode := map[string]meshview.AgentTask{}
	for _, tk := range all {
		byNode[tk.Node] = tk
	}
	if byNode["cloud-1"].Stale {
		t.Fatalf("fresh task on online node must not be stale: %+v", byNode["cloud-1"])
	}
	if !byNode["mac-mini"].Stale {
		t.Fatalf("task of offline node must be stale: %+v", byNode["mac-mini"])
	}

	// 在线但快照超窗 → stale（扫描失败 ≠ 任务还在跑）。
	rawExec(t, path, `UPDATE agent_tasks SET updated_at = ? WHERE node_id = ?`,
		time.Now().Add(-10*time.Minute).Unix(), n1.ID)
	all, err = q.AgentTasks(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range all {
		if tk.Node == "cloud-1" && !tk.Stale {
			t.Fatalf("snapshot past taskStaleAfter must be stale: %+v", tk)
		}
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"stale"`) {
		t.Fatalf("json contract missing stale key: %s", b)
	}
}
