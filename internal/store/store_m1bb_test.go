package store

// M1b-b 新增能力的 store 层单测：tailnet 全量替换、只读句柄守卫、
// 指标聚合与 sparkline 序列。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fptr(f float64) *float64 { return &f }
func iptr(i int64) *int64     { return &i }

func sqlNull(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// openTestStoreAt 打开指定路径的库（只读测试需要跨句柄复用同一文件）。
func openTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// seedNode 注册一个节点并写一条心跳（返回 node id）。
func seedNode(t *testing.T, ctx context.Context, st *Store, name string) int64 {
	t.Helper()
	n, err := st.RegisterNode(ctx, name, "server", "linux", "amd64", "hash-"+name, "reghash-"+name, 0)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := st.HeartbeatFull(ctx, &MetricsRow{
		NodeID: n.ID, TS: time.Now().Unix(),
		CPUPct: fptr(12.5), MemUsed: iptr(1 << 30), MemTotal: iptr(4 << 30),
		DiskUsed: iptr(10 << 30), DiskTotal: iptr(100 << 30),
	}, "test-agent", nil, nil, nil, false)
	if err != nil || !ok {
		t.Fatalf("heartbeat: ok=%v err=%v", ok, err)
	}
	return n.ID
}

// TestReplaceTailnetNodesFullReplace 全量替换：同表两次写入后内容等于第二次
// 集合（事务先清后插，无幽灵行）；updated_at 由 store 统一取 now。
func TestReplaceTailnetNodesFullReplace(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	first := []TailnetNodeRow{
		{ID: 1, MachineName: "cloud-1", IPs: "100.64.0.1", Online: true,
			LastSeen: sqlNull(1760000000)},
		{ID: 2, MachineName: "mac-mini", IPs: "100.64.0.2"},
	}
	if err := st.ReplaceTailnetNodes(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := []TailnetNodeRow{
		{ID: 2, MachineName: "mac-mini", IPs: "100.64.0.2", Online: true},
		{ID: 3, MachineName: "node-3", IPs: "100.64.0.3"},
	}
	if err := st.ReplaceTailnetNodes(ctx, second); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListTailnetNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != 2 || rows[1].ID != 3 {
		t.Fatalf("rows = %+v, want exactly second set", rows)
	}
	if !rows[0].Online || rows[1].Online {
		t.Fatalf("online flags wrong: %+v", rows)
	}
	// 事务原子性兜底：空集合替换 = 清空表（合法状态：headscale 无节点）。
	if err := st.ReplaceTailnetNodes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListTailnetNodes(ctx)
	if len(rows) != 0 {
		t.Fatalf("empty replace must clear table, got %d rows", len(rows))
	}
}

// TestReplaceTailnetNodesRollbackKeepsOld 插入中途失败（R19-#12）：同批第二行
// 触发主键冲突令事务在 DELETE 之后失败 → 整体回滚，旧全量数据必须原样保留
// （先清后插的 DELETE 绝不能因中途失败而单独生效）。
func TestReplaceTailnetNodesRollbackKeepsOld(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	old := []TailnetNodeRow{
		{ID: 1, MachineName: "cloud-1", IPs: "100.64.0.1", Online: true},
		{ID: 2, MachineName: "mac-mini", IPs: "100.64.0.2"},
	}
	if err := st.ReplaceTailnetNodes(ctx, old); err != nil {
		t.Fatal(err)
	}
	bad := []TailnetNodeRow{
		{ID: 3, MachineName: "new-1", IPs: "100.64.0.3", Online: true},
		{ID: 3, MachineName: "dup-id", IPs: "100.64.0.4"}, // 主键冲突：第二行插入失败
	}
	if err := st.ReplaceTailnetNodes(ctx, bad); err == nil {
		t.Fatal("duplicate id in batch must fail the replace")
	}
	rows, err := st.ListTailnetNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != 1 || rows[1].ID != 2 ||
		rows[0].MachineName != "cloud-1" || rows[1].MachineName != "mac-mini" {
		t.Fatalf("mid-batch failure must roll back and keep old rows intact, got %+v", rows)
	}
}

// TestMetricsStatsZeroTotals total=0 的行不再整行剔除（R19-#8）：CPU 等可用
// 指标照常参与聚合，mem/disk 占比按 NULLIF(total,0) 分指标保持 NULL、不除零。
func TestMetricsStatsZeroTotals(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedNode(t, ctx, st, "cloud-1")
	now := time.Now().Unix()
	// cpu 有效但 mem_total=0 / disk_total=0 的行：旧实现被 WHERE 整行剔除，
	// CPU 均值会漏样本（12.5 而非 31.25）。
	if _, err := st.write.ExecContext(ctx,
		`INSERT INTO metrics(node_id, ts, cpu_pct, mem_used, mem_total, disk_used, disk_total) VALUES(?,?,?,?,?,?,?)`,
		id, now-60, fptr(50), iptr(1<<30), 0, iptr(1<<30), 0); err != nil {
		t.Fatal(err)
	}
	stats, err := st.MetricsStatsSince(ctx, id, now-3600)
	if err != nil {
		t.Fatal(err)
	}
	// 样本数 = 窗口内全部行数（含 total=0 行）。
	if stats.Samples != 2 {
		t.Fatalf("samples = %d, want 2", stats.Samples)
	}
	// CPU：seed 12.5 + 50 → 均值 31.25、峰值 50（total=0 行参与 CPU 聚合）。
	if !stats.CPUAvg.Valid || stats.CPUAvg.Float64 != 31.25 {
		t.Fatalf("cpu avg = %+v, want 31.25", stats.CPUAvg)
	}
	if !stats.CPUMax.Valid || stats.CPUMax.Float64 != 50 {
		t.Fatalf("cpu max = %+v, want 50", stats.CPUMax)
	}
	// mem/disk 占比只由 seed 行贡献（25% / 10%），total=0 样本按 NULL 跳过。
	if !stats.MemPctAvg.Valid || stats.MemPctAvg.Float64 != 25 {
		t.Fatalf("mem avg = %+v, want 25 (seed row only)", stats.MemPctAvg)
	}
	if !stats.MemPctMax.Valid || stats.MemPctMax.Float64 != 25 {
		t.Fatalf("mem max = %+v, want 25", stats.MemPctMax)
	}
	if !stats.DiskPctMax.Valid || stats.DiskPctMax.Float64 != 10 {
		t.Fatalf("disk max = %+v, want 10 (seed row only)", stats.DiskPctMax)
	}
}

// TestOpenReadOnlyBlocksWrites 只读句柄（MCP/面板用）：写路径一律报
// ErrReadOnly 而非 panic；mode=ro 连接上绕过 store 直写同样被 SQLite 拒绝
// （query_only=1）。
func TestOpenReadOnlyBlocksWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro.db")
	rw := openTestStoreAt(t, path)
	seedNode(t, ctx, rw, "cloud-1")
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	if _, err := ro.RegisterNode(ctx, "x", "", "", "", "h", "rh", 0); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("RegisterNode err = %v, want read-only error", err)
	}
	if _, err := ro.HeartbeatFull(ctx, &MetricsRow{NodeID: 1}, "v", nil, nil, nil, false); err == nil {
		t.Fatal("HeartbeatFull must fail on read-only handle")
	}
	if err := ro.ReplaceTailnetNodes(ctx, nil); err == nil {
		t.Fatal("ReplaceTailnetNodes must fail on read-only handle")
	}
	// 读路径正常可用。
	nodes, err := ro.ListNodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes on ro handle: %v %v", nodes, err)
	}
}

// TestMetricsStatsAndSeries 指标摘要（24h 窗口口径）与 sparkline 序列：
// 均值/峰值正确、无数据指标保持 NULL、序列按时间升序且限条数。
func TestMetricsStatsAndSeries(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := seedNode(t, ctx, st, "cloud-1")
	now := time.Now().Unix()
	// 手工补三条历史样本（直写 metrics：测试构造专用，正常路径走 Heartbeat）。
	rows := []struct {
		ts   int64
		cpu  *float64
		memU *int64
		memT *int64
	}{
		{now - 300, fptr(10), iptr(1 << 30), iptr(4 << 30)},
		{now - 200, fptr(30), iptr(2 << 30), iptr(4 << 30)},
		{now - 100, nil, iptr(3 << 30), iptr(4 << 30)}, // cpu 缺失 → AVG/MAX 忽略该行
	}
	for _, r := range rows {
		if _, err := st.write.ExecContext(ctx,
			`INSERT INTO metrics(node_id, ts, cpu_pct, mem_used, mem_total) VALUES(?,?,?,?,?)`,
			id, r.ts, r.cpu, r.memU, r.memT); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := st.MetricsStatsSince(ctx, id, now-3600)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Samples != 4 { // seed 心跳一条 + 手工三条
		t.Fatalf("samples = %d, want 4", stats.Samples)
	}
	// cpu: seed 12.5 + 10 + 30 → avg 17.5, max 30；NULL 行不参与。
	if !stats.CPUAvg.Valid || stats.CPUAvg.Float64 != 17.5 {
		t.Fatalf("cpu avg = %+v, want 17.5", stats.CPUAvg)
	}
	if !stats.CPUMax.Valid || stats.CPUMax.Float64 != 30 {
		t.Fatalf("cpu max = %+v, want 30", stats.CPUMax)
	}
	// mem pct：seed 25 + 25 + 50 + 75 → avg 43.75，max 75。
	if !stats.MemPctAvg.Valid || stats.MemPctAvg.Float64 != 43.75 {
		t.Fatalf("mem avg = %+v, want 43.75", stats.MemPctAvg)
	}
	if !stats.MemPctMax.Valid || stats.MemPctMax.Float64 != 75 {
		t.Fatalf("mem max = %+v, want 75", stats.MemPctMax)
	}
	// 窗口外样本不参与。
	out, err := st.MetricsStatsSince(ctx, id, now+10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Samples != 0 || out.CPUAvg.Valid {
		t.Fatalf("future window should be empty, got %+v", out)
	}
	// 序列：限 2 条 → 最新两条（now-200 与 now-100），升序返回。
	pts, err := st.MetricsSeriesSince(ctx, id, now-3600, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || pts[0].TS >= pts[1].TS {
		t.Fatalf("series = %+v, want 2 points ascending", pts)
	}
	if !pts[0].CPUPct.Valid || pts[0].CPUPct.Float64 != 30 {
		t.Fatalf("series[0] = %+v, want cpu 30 (now-200)", pts[0])
	}
	if pts[1].CPUPct.Valid {
		t.Fatalf("series[1] cpu should be NULL (now-100 has no cpu), got %+v", pts[1].CPUPct)
	}
	// 新鲜度。
	ts, ok, err := st.LatestMetricsTime(ctx)
	if err != nil || !ok || ts < now-3600 {
		t.Fatalf("freshness = %d %v %v", ts, ok, err)
	}
}
