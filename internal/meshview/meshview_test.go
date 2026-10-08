package meshview_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mtl802/meshconsole/internal/meshview"
	"github.com/mtl802/meshconsole/internal/store"
)

// seed 建库并布好两节点：cloud-1 在线（带 metrics + 服务 + agent）、
// mac-mini 离线（遗留一行陈旧 inactive 服务）。
func seed(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	n1, err := st.RegisterNode(ctx, "cloud-1", "server", "linux", "amd64", "h1", "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	cpu := 33.5
	memU, memT, du, dt := int64(1<<30), int64(4<<30), int64(10<<30), int64(100<<30)
	_, err = st.HeartbeatFull(ctx, &store.MetricsRow{
		NodeID: n1.ID, TS: time.Now().Unix(),
		CPUPct: &cpu, MemUsed: &memU, MemTotal: &memT, DiskUsed: &du, DiskTotal: &dt,
	}, "agent-1",
		&[]store.ServiceRow{
			{Name: "headscale", Type: "systemd", Target: "headscale.service", Status: "active"},
			{Name: "nextcloud", Type: "docker", Target: "nextcloud-app", Status: "inactive"},
		},
		&[]store.AgentRow{
			{Name: "zcode", Type: "cli", Version: "3.14.4", Status: "active"},
		})
	if err != nil {
		t.Fatal(err)
	}

	n2, err := st.RegisterNode(ctx, "mac-mini", "node", "darwin", "arm64", "h2", "r2", 0)
	if err != nil {
		t.Fatal(err)
	}
	// mac-mini 超过 offline 窗口 → 巡检判 offline。
	if err := st.SetNodeLastSeen(ctx, n2.ID, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SweepOffline(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// mac-mini 遗留服务（替换时 updated_at = now，稍后用原始 SQL 拨旧模拟陈旧行）。
	_, err = st.ReplaceNodeServices(ctx, n2.ID, []store.ServiceRow{
		{Name: "old-svc", Type: "process", Target: "old", Status: "inactive"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, path
}

// staleUpdatedDates 用原始 SQL 把服务行 updated_at 拨到过去（构造陈旧数据态；
// 只读视图层不提供改写路径是特性，测试直连库）。
func backdateService(t *testing.T, path, name string, age time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE services SET updated_at = ? WHERE name = ?`,
		time.Now().Add(-age).Unix(), name)
	if err != nil {
		t.Fatal(err)
	}
}

// TestStatusServiceIssueGrace 服务异常口径（SPEC-M1b-b §2）：
// status != active 且数据未过宽限 → 异常名单；过宽限（陈旧）与 stale 不计。
func TestStatusServiceIssueGrace(t *testing.T) {
	st, path := seed(t)
	q := meshview.New(st)
	ctx := context.Background()

	// 未拨旧：nextcloud inactive 且新鲜 → 异常；old-svc inactive 但同样新鲜 → 也计。
	status, err := q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.NodesTotal != 2 || status.NodesOnline != 1 {
		t.Fatalf("nodes = %d/%d, want 2 total 1 online", status.NodesTotal, status.NodesOnline)
	}
	if len(status.OfflineNodes) != 1 || status.OfflineNodes[0] != "mac-mini" {
		t.Fatalf("offline = %v", status.OfflineNodes)
	}
	var issueNames []string
	for _, is := range status.ServiceIssues {
		issueNames = append(issueNames, is.Node+"/"+is.Name)
	}
	if len(issueNames) != 2 {
		t.Fatalf("fresh inactive rows should both be issues, got %v", issueNames)
	}

	// 拨旧 old-svc（超过 300s 宽限）→ 不再计异常（离线节点的陈旧数据）。
	backdateService(t, path, "old-svc", 10*time.Minute)
	status, err = q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.ServiceIssues) != 1 || status.ServiceIssues[0].Name != "nextcloud" {
		t.Fatalf("stale row must drop out of issues, got %+v", status.ServiceIssues)
	}
	// 同一宽限口径必须贯穿 overview（R19-#4：面板异常数与 get_mesh_status
	// 同源，都在 service 层算好）。
	ov, err := q.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.ServiceIssues) != 1 || ov.ServiceIssues[0].Name != "nextcloud" {
		t.Fatalf("overview issues must follow the same grace cutoff, got %+v", ov.ServiceIssues)
	}
}

// TestStatusStaleExcluded stale 行（配置移除）不进异常名单。
func TestStatusStaleExcluded(t *testing.T) {
	st, _ := seed(t)
	q := meshview.New(st)
	ctx := context.Background()
	// cloud-1 重报服务清单：去掉 nextcloud → 其行转 stale。
	_, err := st.ReplaceNodeServices(ctx, 1, []store.ServiceRow{
		{Name: "headscale", Type: "systemd", Target: "headscale.service", Status: "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := q.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range status.ServiceIssues {
		if is.Name == "nextcloud" {
			t.Fatalf("stale row must not be an issue: %+v", status.ServiceIssues)
		}
	}
}

// TestOverviewAggregation overview 聚合（面板契约）：节点卡带 latest+spark、
// 服务/agent 带 node 名、tailnet 概况、freshness。JSON 序列化走通（面板 JS 消费）。
func TestOverviewAggregation(t *testing.T) {
	st, _ := seed(t)
	q := meshview.New(st)
	ctx := context.Background()

	// tailnet 数据并入。
	err := st.ReplaceTailnetNodes(ctx, []store.TailnetNodeRow{
		{ID: 1, MachineName: "cloud-1", IPs: "100.64.0.1", Online: true,
			LastSeen: sql.NullInt64{Int64: time.Now().Unix() - 60, Valid: true}},
		{ID: 2, MachineName: "mac-mini", IPs: "100.64.0.2", Online: false},
	})
	if err != nil {
		t.Fatal(err)
	}

	ov, err := q.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(ov.Nodes))
	}
	// R19-#2：节点视图必须带 id（temp 库注册顺序固定 cloud-1=1 / mac-mini=2）。
	for _, card := range ov.Nodes {
		if card.Node.ID == 0 {
			t.Fatalf("node view missing id: %+v", card.Node)
		}
		if card.Node.Name == "cloud-1" && card.Node.ID != 1 {
			t.Fatalf("cloud-1 id = %d, want 1", card.Node.ID)
		}
	}
	// lead 排序在面板 JS 侧做；这里只验证字段完整。
	var lead *meshview.NodeCard
	for i := range ov.Nodes {
		if ov.Nodes[i].Node.Name == "cloud-1" {
			lead = &ov.Nodes[i]
		}
	}
	if lead == nil || lead.Latest == nil || lead.Latest.CPUPct == nil || *lead.Latest.CPUPct != 33.5 {
		t.Fatalf("cloud-1 card latest = %+v", lead)
	}
	// R19-#3：每节点卡带 24h 摘要（复用 MetricsStatsSince，与 get_node 同源）。
	if lead.Metrics == nil || lead.Metrics.Samples < 1 || lead.Metrics.CPUAvg == nil || *lead.Metrics.CPUAvg != 33.5 {
		t.Fatalf("cloud-1 card metrics summary = %+v", lead.Metrics)
	}
	if len(lead.Spark) == 0 {
		t.Fatal("spark series empty")
	}
	// R19-#4：overview 直接输出异常名单（两行新鲜 inactive：nextcloud + old-svc）。
	if len(ov.ServiceIssues) != 2 {
		t.Fatalf("service issues = %+v, want 2 fresh inactive rows", ov.ServiceIssues)
	}
	if len(ov.Services) != 3 || len(ov.Agents) != 1 { // cloud-1 两条 + mac-mini 遗留一条
		t.Fatalf("services=%d agents=%d", len(ov.Services), len(ov.Agents))
	}
	if ov.Services[0].Node == "" {
		t.Fatal("service rows must carry node name")
	}
	if ov.Agents[0].Invokable {
		t.Fatal("invokable must be false (store-enforced)")
	}
	if ov.Tailnet == nil || ov.Tailnet.Tracked != 2 || ov.Tailnet.Online != 1 {
		t.Fatalf("tailnet = %+v", ov.Tailnet)
	}
	if ov.Freshness == nil {
		t.Fatal("freshness missing")
	}
	// JSON 契约可序列化且键名符合面板/MCP 消费。
	b, err := json.Marshal(ov)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"generated_at"`, `"nodes"`, `"id"`, `"metrics"`, `"services"`, `"service_issues"`, `"agents"`, `"tailnet"`, `"machine_name"`, `"invokable"`} {
		if !jsonContains(string(b), key) {
			t.Fatalf("overview json missing %s: %s", key, b)
		}
	}
}

func jsonContains(s, sub string) bool {
	return strings.Contains(s, sub)
}
