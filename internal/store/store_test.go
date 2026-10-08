package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMigrations(t *testing.T) {
	st := openTestStore(t)
	var version int
	if err := st.write.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("migration version = %d, want %d", version, len(migrations))
	}
	for _, table := range []string{"nodes", "metrics", "consumed_registration_tokens", "services", "ai_agents"} {
		var name string
		err := st.write.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	// WAL 必须生效。
	var mode string
	if err := st.write.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q (err=%v), want wal", mode, err)
	}
}

func TestRegisterAndAuthenticate(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	node, err := st.RegisterNode(ctx, "mac-mini", "workstation", "darwin", "arm64", "hash-a", "reg-a", 0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if node.Name != "mac-mini" || node.Status != "online" {
		t.Fatalf("unexpected node: %+v", node)
	}

	got, err := st.AuthenticateNode(ctx, "hash-a")
	if err != nil || got == nil {
		t.Fatalf("authenticate: node=%v err=%v", got, err)
	}
	if got.ID != node.ID {
		t.Fatalf("authenticate id = %d, want %d", got.ID, node.ID)
	}
	missing, err := st.AuthenticateNode(ctx, "hash-unknown")
	if err != nil || missing != nil {
		t.Fatalf("unknown token: node=%v err=%v, want nil,nil", missing, err)
	}
}

func TestRegisterDuplicateName(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.RegisterNode(ctx, "n1", "", "darwin", "arm64", "h1", "reg-1", 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// 重名拒绝，且第二个 token 不应被烧掉（事务回滚语义由重复防护路径保证）。
	if _, err := st.RegisterNode(ctx, "n1", "", "linux", "amd64", "h2", "reg-2", 0); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("duplicate register err = %v, want ErrDuplicateNode", err)
	}
	// 换名同 token：h2 未被重复注册路径消耗，仍可正常注册。
	if _, err := st.RegisterNode(ctx, "n2", "", "linux", "amd64", "h2", "reg-2", 0); err != nil {
		t.Fatalf("register with unused token: %v", err)
	}
}

func TestRegisterTokenSingleUse(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.RegisterNode(ctx, "a", "", "os", "arch", "hash-a", "reg-same", 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := st.RegisterNode(ctx, "b", "", "os", "arch", "hash-b", "reg-same", 0); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("reused token err = %v, want ErrTokenUsed", err)
	}
	// 消费校验必须先于重名检查（审查 R1-#3）：已消费 token 撞上已占用节点名
	// 也必须报 ErrTokenUsed，否则可借「重名/未占用」差异探测节点名存在性。
	if _, err := st.RegisterNode(ctx, "a", "", "os", "arch", "hash-c", "reg-same", 0); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("consumed token + dup name err = %v, want ErrTokenUsed", err)
	}
}

// TestRegisterTokenExpiredInTx 消费前事务内二次校验到期（R3）：认证中间件的
// 到期检查发生在读 body 之前，事务内在烧 token 前再挡一次跨到期时刻的窗口。
func TestRegisterTokenExpiredInTx(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Minute).Unix()
	if _, err := st.RegisterNode(ctx, "n1", "", "os", "arch", "h1", "reg-e", past); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token err = %v, want ErrTokenExpired", err)
	}
	// 恰好在到期时刻（now == expires_at）同样视为过期。
	if _, err := st.RegisterNode(ctx, "n1", "", "os", "arch", "h2", "reg-now", time.Now().Unix()); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("at-expiry token err = %v, want ErrTokenExpired", err)
	}
	// 过期拒绝发生在烧 token 之前：同一 token 换有效到期后仍可正常注册。
	future := time.Now().Add(time.Hour).Unix()
	if _, err := st.RegisterNode(ctx, "n1", "", "os", "arch", "h1", "reg-e", future); err != nil {
		t.Fatalf("register with valid expiry: %v", err)
	}
}

// TestRegisterNodeNowInsideTx 钉住 R5 修复：到期判定时钟取在 BeginTx 之后的事务
// 内。占住唯一写连接（SetMaxOpenConns(1)）令 RegisterNode 的 BeginTx 排队，到期
// 时刻定在排队期间——释放锁后事务实际执行时必已过期，须拒绝；若 now 取在
// BeginTx 之前（修复前写法）则排队前判定未过期，该用例必失败。
func TestRegisterNodeNowInsideTx(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// 占住唯一写连接，把 RegisterNode 卡在 BeginTx 排队上。
	held, err := st.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("hold write conn: %v", err)
	}
	expires := time.Now().Add(300 * time.Millisecond).Unix()
	done := make(chan error, 1)
	go func() {
		_, err := st.RegisterNode(ctx, "late", "", "os", "arch", "h-late", "reg-late", expires)
		done <- err
	}()
	time.Sleep(700 * time.Millisecond) // 跨过到期时刻，仍持锁
	if err := held.Rollback(); err != nil {
		t.Fatalf("release write conn: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("token expired during queue wait: err = %v, want ErrTokenExpired", err)
	}
	// 排队期间的过期拒绝不误耗 token：换有效到期后同一 token 可正常注册。
	if _, err := st.RegisterNode(ctx, "late", "", "os", "arch", "h-late", "reg-late",
		time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("register after expired reject: %v", err)
	}
}

func TestHeartbeat(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	node, err := st.RegisterNode(ctx, "n1", "role", "os", "arch", "h1", "reg-1", 0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	before := node.LastSeen.Int64
	time.Sleep(1100 * time.Millisecond) // 保证 unix 秒前进

	// 全部采集成功：last_seen 与 last_success 同时刷新。
	pct := 12.5
	mem := int64(1024)
	ok, err := st.Heartbeat(ctx, &MetricsRow{
		NodeID: node.ID, TS: time.Now().Unix(),
		CPUPct: &pct, MemUsed: &mem, MemTotal: &mem,
	}, "v0.1.0")
	if err != nil || !ok {
		t.Fatalf("heartbeat: ok=%v err=%v", ok, err)
	}
	got, _ := st.GetNodeByName(ctx, "n1")
	if !got.LastSeen.Valid || got.LastSeen.Int64 <= before {
		t.Fatalf("last_seen not refreshed: %v", got.LastSeen)
	}
	if !got.LastSuccess.Valid {
		t.Fatal("last_success not set on clean heartbeat")
	}
	if got.AgentVersion != "v0.1.0" {
		t.Fatalf("agent_version = %q", got.AgentVersion)
	}
	if n, _ := st.CountMetrics(ctx, node.ID); n != 1 {
		t.Fatalf("metrics count = %d, want 1", n)
	}

	// 带采集错误：last_seen 刷新但 last_success 保持不动（DESIGN §4.1 双时间戳语义）。
	prevSuccess := got.LastSuccess.Int64
	time.Sleep(1100 * time.Millisecond)
	ok, err = st.Heartbeat(ctx, &MetricsRow{
		NodeID: node.ID, TS: time.Now().Unix(),
		CollectErrors: `{"load1":"not implemented"}`,
	}, "v0.1.0")
	if err != nil || !ok {
		t.Fatalf("heartbeat with errors: ok=%v err=%v", ok, err)
	}
	got, _ = st.GetNodeByName(ctx, "n1")
	if got.LastSuccess.Int64 != prevSuccess {
		t.Fatalf("last_success changed despite collect errors")
	}
	if got.LastSeen.Int64 <= prevSuccess {
		t.Fatal("last_seen should still refresh on partial-collect heartbeat")
	}
}

func TestHeartbeatNullFieldsStoredAsNull(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	node, err := st.RegisterNode(ctx, "n1", "", "os", "arch", "h1", "reg-1", 0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// 所有指针为 nil（采集失败）→ 库里必须是 NULL，不得出现 0。
	if _, err := st.Heartbeat(ctx, &MetricsRow{
		NodeID: node.ID, TS: time.Now().Unix(),
		CollectErrors: `{"cpu_pct":"x","load1":"y"}`,
	}, "v1"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	var cpu, load1, memUsed sql.NullFloat64
	if err := st.read.QueryRow(
		`SELECT cpu_pct, load1, mem_used FROM metrics WHERE node_id=?`, node.ID,
	).Scan(&cpu, &load1, &memUsed); err != nil {
		t.Fatalf("query metrics: %v", err)
	}
	if cpu.Valid || load1.Valid || memUsed.Valid {
		t.Fatalf("null fields stored as values: cpu=%v load1=%v mem=%v", cpu, load1, memUsed)
	}
}

func TestSweepOffline(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterNode(ctx, "fresh", "", "os", "arch", "hf", "reg-f", 0); err != nil {
		t.Fatalf("register fresh: %v", err)
	}
	stale, err := st.RegisterNode(ctx, "stale", "", "os", "arch", "hs", "reg-s", 0)
	if err != nil {
		t.Fatalf("register stale: %v", err)
	}
	// 把 stale 的 last_seen 拨到 120s 前。
	if _, err := st.write.Exec(`UPDATE nodes SET last_seen = ? WHERE id = ?`,
		time.Now().Unix()-120, stale.ID); err != nil {
		t.Fatalf("age stale node: %v", err)
	}

	cutoff := time.Now().Add(-60 * time.Second).Unix()
	n, err := st.SweepOffline(ctx, cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept = %d, want 1", n)
	}
	got, _ := st.GetNodeByName(ctx, "stale")
	if got.Status != "offline" {
		t.Fatalf("stale status = %q, want offline", got.Status)
	}
	keep, _ := st.GetNodeByName(ctx, "fresh")
	if keep.Status != "online" {
		t.Fatalf("fresh status = %q, want online", keep.Status)
	}
	// 幂等：已 offline 的不再重复计数。
	if n, _ := st.SweepOffline(ctx, cutoff); n != 0 {
		t.Fatalf("second sweep = %d, want 0", n)
	}
}

func TestCleanupMetrics(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	node, err := st.RegisterNode(ctx, "n1", "", "os", "arch", "h1", "reg-1", 0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	now := time.Now().Unix()
	for _, ts := range []int64{old, old + 1, now, now + 1} {
		if _, err := st.Heartbeat(ctx, &MetricsRow{NodeID: node.ID, TS: ts}, "v"); err != nil {
			t.Fatalf("insert metrics ts=%d: %v", ts, err)
		}
	}
	deleted, err := st.CleanupMetrics(ctx, time.Now().Add(-7*24*time.Hour).Unix())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if n, _ := st.CountMetrics(ctx, node.ID); n != 2 {
		t.Fatalf("remaining = %d, want 2", n)
	}
}

// TestReplaceNodeServices 全量替换 + stale 语义（SPEC-M1b-a §2/§4）。
func TestReplaceNodeServices(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	node, err := st.RegisterNode(ctx, "n1", "", "darwin", "arm64", "h1", "r1", 0)
	if err != nil {
		t.Fatal(err)
	}

	// 首次上报 2 个服务。
	ok, err := st.ReplaceNodeServices(ctx, node.ID, []ServiceRow{
		{Name: "svc-a", Type: "process", Target: "a", Status: "active"},
		{Name: "svc-b", Type: "systemd", Target: "b.service", Status: "inactive", Detail: "is-active: inactive"},
	})
	if err != nil || !ok {
		t.Fatalf("replace: ok=%v err=%v", ok, err)
	}
	rows, err := st.ListServices(ctx, node.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %v rows=%d", err, len(rows))
	}

	// 第二次上报只剩 svc-a：svc-b 变 stale（保留行不删除），svc-a 状态刷新。
	ok, err = st.ReplaceNodeServices(ctx, node.ID, []ServiceRow{
		{Name: "svc-a", Type: "process", Target: "a", Status: "failed", Detail: "is-active: failed"},
	})
	if err != nil || !ok {
		t.Fatalf("replace2: ok=%v err=%v", ok, err)
	}
	rows, _ = st.ListServices(ctx, node.ID)
	if len(rows) != 2 {
		t.Fatalf("stale row must be kept, rows=%d", len(rows))
	}
	byName := map[string]ServiceRecord{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if byName["svc-a"].Status != "failed" || byName["svc-a"].Detail != "is-active: failed" {
		t.Fatalf("svc-a not upserted: %+v", byName["svc-a"])
	}
	if byName["svc-b"].Status != "stale" || byName["svc-b"].Target != "b.service" {
		t.Fatalf("svc-b should be stale with history kept: %+v", byName["svc-b"])
	}

	// 空列表上报：全部转 stale。
	ok, err = st.ReplaceNodeServices(ctx, node.ID, nil)
	if err != nil || !ok {
		t.Fatalf("replace empty: ok=%v err=%v", ok, err)
	}
	rows, _ = st.ListServices(ctx, node.ID)
	for _, r := range rows {
		if r.Status != "stale" {
			t.Fatalf("all rows should be stale after empty report: %+v", r)
		}
	}

	// 复活：stale 行随上报恢复真实状态（UPSERT 覆盖）。
	ok, _ = st.ReplaceNodeServices(ctx, node.ID, []ServiceRow{
		{Name: "svc-b", Type: "systemd", Target: "b.service", Status: "active"},
	})
	rows, _ = st.ListServices(ctx, node.ID)
	byName = map[string]ServiceRecord{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if !ok || byName["svc-b"].Status != "active" || byName["svc-a"].Status != "stale" {
		t.Fatalf("revive wrong: ok=%v a=%+v b=%+v", ok, byName["svc-a"], byName["svc-b"])
	}

	// 节点不存在：返回 false，不写行。
	ok, err = st.ReplaceNodeServices(ctx, 99999, []ServiceRow{{Name: "x", Type: "process", Status: "active"}})
	if ok || err != nil {
		t.Fatalf("missing node: ok=%v err=%v", ok, err)
	}
}

// TestReplaceNodeAgents ai_agents 全量替换 + invokable 服务端强制 false。
func TestReplaceNodeAgents(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	node, _ := st.RegisterNode(ctx, "n1", "", "darwin", "arm64", "h1", "r1", 0)

	ok, err := st.ReplaceNodeAgents(ctx, node.ID, []AgentRow{
		{Name: "zcode", Type: "cli", Version: "1.0", Path: "/usr/local/bin/zcode", Status: "active"},
		{Name: "hana-agent", Type: "service", Status: "unavailable", Detail: "port 5800 closed"},
	})
	if err != nil || !ok {
		t.Fatalf("replace: ok=%v err=%v", ok, err)
	}
	rows, err := st.ListAgents(ctx, node.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %v rows=%d", err, len(rows))
	}
	for _, r := range rows {
		if r.Invokable {
			t.Fatalf("invokable must be forced false: %+v", r)
		}
	}

	// 声明优先的覆盖：同名 zcode 新版本与路径。
	ok, _ = st.ReplaceNodeAgents(ctx, node.ID, []AgentRow{
		{Name: "zcode", Type: "cli", Version: "2.0", Path: "/opt/tools/zcode", Status: "active"},
	})
	rows, _ = st.ListAgents(ctx, node.ID)
	if len(rows) != 2 {
		t.Fatalf("stale agent row must be kept, rows=%d", len(rows))
	}
	byName := map[string]AgentRecord{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if byName["zcode"].Version != "2.0" || byName["zcode"].Path != "/opt/tools/zcode" {
		t.Fatalf("zcode not upserted: %+v", byName["zcode"])
	}
	if byName["hana-agent"].Status != "stale" {
		t.Fatalf("hana-agent should be stale: %+v", byName["hana-agent"])
	}

	// 节点删除级联清理（foreign_keys=ON）。
	if n, err := st.DeleteNode(ctx, node.ID); err != nil || n != 1 {
		t.Fatalf("delete node: %v err=%v", n, err)
	}
	var count int
	if err := st.read.QueryRow(`SELECT COUNT(*) FROM ai_agents`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("ai_agents not cascade-deleted: count=%d err=%v", count, err)
	}
}

// TestHeartbeatFullAtomic HeartbeatFull 单事务（R11-F）：一次调用完成 metrics +
// services + agents 全部写入；中途失败整体回滚——失败注入用超大批次服务名
// （4 万个占位符超过 SQLite 默认变量数上限 32766，markStale 的 NOT IN 报错），
// 此刻 metrics 已在事务内写入，断言其被回滚不落盘。
func TestHeartbeatFullAtomic(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	node, err := st.RegisterNode(ctx, "n1", "", "os", "arch", "h1", "reg-1", 0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	pct := 12.5
	row := &MetricsRow{NodeID: node.ID, TS: time.Now().Unix(), CPUPct: &pct}
	svcs := []ServiceRow{{Name: "a", Type: "process", Target: "a", Status: "active"}}
	ags := []AgentRow{{Name: "zcode", Type: "cli", Version: "1.0", Status: "active"}}
	if ok, err := st.HeartbeatFull(ctx, row, "v1", &svcs, &ags, nil, false, nil, nil); err != nil || !ok {
		t.Fatalf("heartbeat full: ok=%v err=%v", ok, err)
	}
	if n, _ := st.CountMetrics(ctx, node.ID); n != 1 {
		t.Fatalf("metrics = %d, want 1", n)
	}
	if s, _ := st.ListServices(ctx, node.ID); len(s) != 1 || s[0].Name != "a" {
		t.Fatalf("services = %+v, want single row a", s)
	}
	if a, _ := st.ListAgents(ctx, node.ID); len(a) != 1 || a[0].Name != "zcode" {
		t.Fatalf("agents = %+v, want single row zcode", a)
	}

	// 缺席语义：nil = 字段缺席，既有清单不动、metrics 照写。
	if ok, err := st.HeartbeatFull(ctx, row, "v1", nil, nil, nil, false, nil, nil); err != nil || !ok {
		t.Fatalf("heartbeat full (absent): ok=%v err=%v", ok, err)
	}
	if s, _ := st.ListServices(ctx, node.ID); len(s) != 1 || s[0].Status != "active" {
		t.Fatalf("nil services must not touch existing rows: %+v", s)
	}
	if n, _ := st.CountMetrics(ctx, node.ID); n != 2 {
		t.Fatalf("metrics = %d, want 2", n)
	}

	// 中途失败注入：4 万行服务批次令 markStale 的 NOT IN 占位符超限报错。
	flood := make([]ServiceRow, 40000)
	for i := range flood {
		flood[i] = ServiceRow{Name: fmt.Sprintf("s%05d", i), Type: "process", Status: "active"}
	}
	if _, err := st.HeartbeatFull(ctx, row, "v1", &flood, nil, nil, false, nil, nil); err == nil {
		t.Fatal("oversized services batch must fail")
	}
	// metrics 与既有服务行必须原样保留（半轮数据不存在）。
	if n, _ := st.CountMetrics(ctx, node.ID); n != 2 {
		t.Fatalf("failed heartbeat must roll back metrics: got %d rows, want 2", n)
	}
	if s, _ := st.ListServices(ctx, node.ID); len(s) != 1 || s[0].Name != "a" {
		t.Fatalf("failed heartbeat must roll back services: %+v", s)
	}
}
