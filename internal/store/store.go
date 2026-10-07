// Package store 为 MeshConsole 的 SQLite 访问层（modernc.org/sqlite 纯 Go）。
//
// 并发口径（规避 SQLITE_BUSY）：
//   - 写句柄 SetMaxOpenConns(1)：单写连接，写事务天然串行；
//   - 读句柄独立连接池：WAL 模式下读写互不阻塞；
//   - busy_timeout=5000ms 兜底（每连接生效）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrDuplicateNode 节点名已存在（重复注册防护）。
var ErrDuplicateNode = errors.New("node name already registered")

// ErrTokenUsed 注册 token 已被使用（一次性 token 用后即废）。
var ErrTokenUsed = errors.New("registration token already consumed")

// ErrTokenExpired 注册 token 已过有效期（消费前事务内二次校验，R3）。
var ErrTokenExpired = errors.New("registration token expired")

// Node 为 nodes 表行。
type Node struct {
	ID           int64
	Name         string
	Role         string
	OS           string
	Arch         string
	TailnetIP    string
	PublicIP     string
	AgentVersion string
	Status       string
	TokenHash    string
	LastSeen     sql.NullInt64 // unix 秒
	LastSuccess  sql.NullInt64 // unix 秒
	CreatedAt    int64         // unix 秒
}

// MetricsRow 为 metrics 表行；空指针字段入库为 NULL（采集失败禁止填 0）。
type MetricsRow struct {
	NodeID        int64
	TS            int64 // unix 秒
	CPUPct        *float64
	MemUsed       *int64
	MemTotal      *int64
	DiskUsed      *int64
	DiskTotal     *int64
	NetRx         *int64
	NetTx         *int64
	UptimeS       *int64
	Load1         *float64
	CollectErrors string // JSON 对象：字段名 -> 错误说明；空串表示全部采集成功
}

// Store 封装读写两个连接池。
type Store struct {
	write *sql.DB
	read  *sql.DB
}

func dsn(path string) string {
	v := url.Values{}
	// 值经驱动按 `PRAGMA <v>` 原样执行；busy_timeout 驱动保证最先应用。
	v.Add("_pragma", "busy_timeout=5000")
	v.Add("_pragma", "journal_mode=WAL")
	v.Add("_pragma", "synchronous=NORMAL")
	v.Add("_pragma", "foreign_keys=ON")
	return "file:" + path + "?" + v.Encode()
}

func openDB(path string, maxOpen int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Open 打开数据库并执行增量 schema migration。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	write, err := openDB(path, 1)
	if err != nil {
		return nil, fmt.Errorf("open write db: %w", err)
	}
	read, err := openDB(path, 4)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("open read db: %w", err)
	}
	s := &Store{write: write, read: read}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	errW := s.write.Close()
	errR := s.read.Close()
	if errW != nil {
		return errW
	}
	return errR
}

// migrations 按版本递增排列；只执行 schema_migrations 中缺失的版本。
var migrations = []struct {
	Version int
	SQL     string
}{
	{
		Version: 1,
		SQL: `
CREATE TABLE IF NOT EXISTS nodes (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT    NOT NULL UNIQUE,
	role          TEXT    NOT NULL DEFAULT '',
	os            TEXT    NOT NULL DEFAULT '',
	arch          TEXT    NOT NULL DEFAULT '',
	tailnet_ip    TEXT    NOT NULL DEFAULT '',
	public_ip     TEXT    NOT NULL DEFAULT '',
	agent_version TEXT    NOT NULL DEFAULT '',
	status        TEXT    NOT NULL DEFAULT 'online',
	token_hash    TEXT    NOT NULL UNIQUE,
	last_seen     INTEGER,
	last_success  INTEGER,
	created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nodes_last_seen ON nodes(last_seen);

CREATE TABLE IF NOT EXISTS metrics (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id        INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	ts             INTEGER NOT NULL,
	cpu_pct        REAL,
	mem_used       INTEGER,
	mem_total      INTEGER,
	disk_used      INTEGER,
	disk_total     INTEGER,
	net_rx         INTEGER,
	net_tx         INTEGER,
	uptime_s       INTEGER,
	load1          REAL,
	collect_errors TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_metrics_ts ON metrics(ts);
CREATE INDEX IF NOT EXISTS idx_metrics_node_ts ON metrics(node_id, ts);

CREATE TABLE IF NOT EXISTS consumed_registration_tokens (
	token_hash  TEXT PRIMARY KEY,
	consumed_at INTEGER NOT NULL
);
`,
	},
	{
		// M1b-a（SPEC §4）：受管服务清单与 AI agent 发现落库。
		// (node_id, name) 唯一：同节点同名 UPSERT；节点删除级联清理。
		// services.status 额外允许 'stale'（服务消失于上报时保留历史行，不删）。
		Version: 2,
		SQL: `
CREATE TABLE IF NOT EXISTS services (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	name       TEXT    NOT NULL,
	type       TEXT    NOT NULL,
	target     TEXT    NOT NULL DEFAULT '',
	status     TEXT    NOT NULL,
	detail     TEXT    NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	UNIQUE(node_id, name)
);
CREATE INDEX IF NOT EXISTS idx_services_node_updated ON services(node_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_services_updated ON services(updated_at);

CREATE TABLE IF NOT EXISTS ai_agents (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	name       TEXT    NOT NULL,
	type       TEXT    NOT NULL,
	version    TEXT    NOT NULL DEFAULT '',
	path       TEXT    NOT NULL DEFAULT '',
	status     TEXT    NOT NULL,
	invokable  INTEGER NOT NULL DEFAULT 0,
	detail     TEXT    NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	UNIQUE(node_id, name)
);
CREATE INDEX IF NOT EXISTS idx_ai_agents_node_updated ON ai_agents(node_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_ai_agents_updated ON ai_agents(updated_at);
`,
	},
}

func (s *Store) migrate() error {
	if _, err := s.write.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	applied_at INTEGER NOT NULL
)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := s.write.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err := s.write.BeginTx(ctx, nil)
		if err != nil {
			cancel()
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			cancel()
			return fmt.Errorf("apply migration %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
			m.Version, time.Now().Unix()); err != nil {
			tx.Rollback()
			cancel()
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			cancel()
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
		cancel()
	}
	return nil
}

// RegisterNode 在单事务内完成：到期二次校验 → 消耗校验一次性注册 token →
// 重名拒绝（不烧 token）→ 插入节点。nodeTokenHash 为签发给节点的 token 哈希
// （存 nodes.token_hash），regTokenHash 为本次使用的注册 token 哈希（消耗键），
// regTokenExpiresAt 为该注册 token 的到期 unix 秒（0 表示无到期，防御缺省）。
// 到期二次校验（R3）：认证中间件的到期检查发生在读取请求体之前，body 传输
// 耗时可能跨越到期时刻，烧 token 前在事务内再挡一次窗口；判定时钟 now 取在
// BeginTx 之后事务内（R5）——单写连接下 BeginTx 可能排队等锁，排队跨过到期
// 时刻也按事务实际执行时刻正确拒绝。
// 消费校验必须先于重名检查：已消费 token 一律 ErrTokenUsed（对外 401），
// 否则攻击者可凭已消费 token 借「重名 409 / 未占用 401」的差异探测节点名存在性。
// 任一步失败整体回滚，未消费的 token 不被误耗。
func (s *Store) RegisterNode(ctx context.Context, name, role, osName, arch, nodeTokenHash, regTokenHash string, regTokenExpiresAt int64) (*Node, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()

	if regTokenExpiresAt != 0 && now >= regTokenExpiresAt {
		return nil, ErrTokenExpired
	}

	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO consumed_registration_tokens(token_hash, consumed_at) VALUES(?, ?)`,
		regTokenHash, now)
	if err != nil {
		return nil, err
	}
	// 0 行受影响 = 唯一键已存在 = token 已被用过。
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrTokenUsed
	}

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE name = ?`, name).Scan(&exists); err == nil {
		return nil, ErrDuplicateNode
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	res, err = tx.ExecContext(ctx, `
INSERT INTO nodes(name, role, os, arch, agent_version, status, token_hash, last_seen, last_success, created_at)
VALUES(?, ?, ?, ?, '', 'online', ?, ?, NULL, ?)`,
		name, role, osName, arch, nodeTokenHash, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Node{
		ID: id, Name: name, Role: role, OS: osName, Arch: arch,
		Status: "online", TokenHash: nodeTokenHash, LastSeen: sql.NullInt64{Int64: now, Valid: true},
		CreatedAt: now,
	}, nil
}

// AuthenticateNode 按节点 token 哈希取节点；未命中返回 nil（调用方一律回 401）。
func (s *Store) AuthenticateNode(ctx context.Context, tokenHash string) (*Node, error) {
	row := s.read.QueryRowContext(ctx, `
SELECT id, name, role, os, arch, tailnet_ip, public_ip, agent_version, status,
       token_hash, last_seen, last_success, created_at
FROM nodes WHERE token_hash = ?`, tokenHash)
	return scanNode(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanNode(row rowScanner) (*Node, error) {
	n := &Node{}
	err := row.Scan(&n.ID, &n.Name, &n.Role, &n.OS, &n.Arch, &n.TailnetIP, &n.PublicIP,
		&n.AgentVersion, &n.Status, &n.TokenHash, &n.LastSeen, &n.LastSuccess, &n.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return n, nil
}

// Heartbeat 刷新 last_seen（无采集错误时同时刷新 last_success，DESIGN §4.1 双时间戳语义）
// 并写入一条 metrics。nodeID 与 nodeName 须与 token 解析出的节点一致（由 registry 层校验）。
// 返回 false 表示节点不存在（token 失效）。
func (s *Store) Heartbeat(ctx context.Context, n *MetricsRow, agentVersion string) (bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ok, err := heartbeatTx(ctx, tx, n, agentVersion, time.Now().Unix())
	if err != nil || !ok {
		return ok, err
	}
	return true, tx.Commit()
}

// heartbeatTx 为心跳的核心写入（节点时间戳 + 一条 metrics），供单条心跳事务与
// HeartbeatFull 组合事务共用。返回 false 表示节点不存在。
func heartbeatTx(ctx context.Context, tx *sql.Tx, n *MetricsRow, agentVersion string, now int64) (bool, error) {
	successRefresh := "last_seen = ?"
	if n.CollectErrors == "" {
		successRefresh = "last_seen = ?, last_success = ?"
	}
	q := "UPDATE nodes SET agent_version = ?, status = 'online', " + successRefresh + " WHERE id = ?"
	var args []any
	if n.CollectErrors == "" {
		args = []any{agentVersion, now, now, n.NodeID}
	} else {
		args = []any{agentVersion, now, n.NodeID}
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO metrics(node_id, ts, cpu_pct, mem_used, mem_total, disk_used, disk_total,
                    net_rx, net_tx, uptime_s, load1, collect_errors)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.NodeID, n.TS, n.CPUPct, n.MemUsed, n.MemTotal, n.DiskUsed, n.DiskTotal,
		n.NetRx, n.NetTx, n.UptimeS, n.Load1, n.CollectErrors); err != nil {
		return false, err
	}
	return true, nil
}

// HeartbeatFull 单事务完成一次心跳的全部写入（R11-F）：节点时间戳 + metrics +
// services 全量替换（services 非 nil 时）+ agents 全量替换（agents 非 nil 时）。
// 任何一步失败整体回滚——三次独立事务中途失败会留下「metrics 已落、清单未换」
// 的半轮数据，合并后与单条心跳同原子性。services/agents 传 nil 表示字段缺席
// （无变化不覆盖）。返回 false 表示节点不存在（token 失效）。
func (s *Store) HeartbeatFull(ctx context.Context, n *MetricsRow, agentVersion string, services *[]ServiceRow, agents *[]AgentRow) (bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	ok, err := heartbeatTx(ctx, tx, n, agentVersion, now)
	if err != nil || !ok {
		return ok, err
	}
	if services != nil {
		if err := replaceServicesTx(ctx, tx, n.NodeID, *services, now); err != nil {
			return false, err
		}
	}
	if agents != nil {
		if err := replaceAgentsTx(ctx, tx, n.NodeID, *agents, now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SweepOffline 将 last_seen 早于 cutoff 的在线节点标记为 offline，返回受影响行数。
func (s *Store) SweepOffline(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE nodes SET status = 'offline' WHERE status = 'online' AND last_seen < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupMetrics 删除 ts 早于 cutoff 的指标行，返回删除行数。
func (s *Store) CleanupMetrics(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.write.ExecContext(ctx, `DELETE FROM metrics WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountMetrics 返回某节点当前指标行数（测试与自检用）。
func (s *Store) CountMetrics(ctx context.Context, nodeID int64) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM metrics WHERE node_id = ?`, nodeID).Scan(&n)
	return n, err
}

// GetNodeByName 按名称取节点（测试与自检用）。
func (s *Store) GetNodeByName(ctx context.Context, name string) (*Node, error) {
	row := s.read.QueryRowContext(ctx, `
SELECT id, name, role, os, arch, tailnet_ip, public_ip, agent_version, status,
       token_hash, last_seen, last_success, created_at
FROM nodes WHERE name = ?`, name)
	return scanNode(row)
}

// SetNodeLastSeen 直接改写节点 last_seen（运维调整/测试构造过期态）。
func (s *Store) SetNodeLastSeen(ctx context.Context, nodeID, unix int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE nodes SET last_seen = ? WHERE id = ?`, unix, nodeID)
	return err
}

// DeleteNode 删除节点及其指标行（管理操作；节点 token 随行失效）。
func (s *Store) DeleteNode(ctx context.Context, nodeID int64) (int64, error) {
	res, err := s.write.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, nodeID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MetricsRecord 为读出的指标行；Null* 保持库中 NULL 语义。
type MetricsRecord struct {
	TS            int64
	CPUPct        sql.NullFloat64
	Load1         sql.NullFloat64
	MemUsed       sql.NullInt64
	MemTotal      sql.NullInt64
	DiskUsed      sql.NullInt64
	DiskTotal     sql.NullInt64
	NetRx         sql.NullInt64
	NetTx         sql.NullInt64
	UptimeS       sql.NullInt64
	CollectErrors string
}

// LatestMetrics 取某节点最新一条指标行；无行返回 nil。
func (s *Store) LatestMetrics(ctx context.Context, nodeID int64) (*MetricsRecord, error) {
	rec := &MetricsRecord{}
	err := s.read.QueryRowContext(ctx, `
SELECT ts, cpu_pct, load1, mem_used, mem_total, disk_used, disk_total,
       net_rx, net_tx, uptime_s, collect_errors
FROM metrics WHERE node_id = ? ORDER BY id DESC LIMIT 1`, nodeID).Scan(
		&rec.TS, &rec.CPUPct, &rec.Load1, &rec.MemUsed, &rec.MemTotal, &rec.DiskUsed,
		&rec.DiskTotal, &rec.NetRx, &rec.NetTx, &rec.UptimeS, &rec.CollectErrors)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ServiceRow 为 services 表行（心跳上报的受管服务状态）。
type ServiceRow struct {
	NodeID    int64
	Name      string
	Type      string
	Target    string
	Status    string // active/inactive/failed/unavailable/unknown；消失于上报由本层写 stale
	Detail    string
	UpdatedAt int64 // unix 秒，由 store 统一取 now
}

// AgentRow 为 ai_agents 表行（心跳上报的 AI agent 发现结果）。
type AgentRow struct {
	NodeID    int64
	Name      string
	Type      string // cli/service
	Version   string
	Path      string
	Status    string // active/inactive/unavailable/unknown
	Invokable bool   // M1 恒 false（发现 ≠ 可调用，DESIGN §4.2-B）
	Detail    string
	UpdatedAt int64 // unix 秒，由 store 统一取 now
}

// ReplaceNodeServices 以本次上报为准全量替换某节点的服务清单（SPEC-M1b-a §4）：
// 上报条目逐条 UPSERT（同节点同名覆盖），本次未出现的历史条目 status 改写为
// 'stale'（保留行与历史，不删除）。单事务完成。
// 返回 false 表示节点不存在（心跳竞态中被删除），调用方按凭据失效处理。
func (s *Store) ReplaceNodeServices(ctx context.Context, nodeID int64, svcs []ServiceRow) (bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// 节点存在性检查：与 metrics 心跳同竞态语义（节点被删后迟到的上报不复活幽灵行）。
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ?`, nodeID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	now := time.Now().Unix()
	if err := replaceServicesTx(ctx, tx, nodeID, svcs, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// replaceServicesTx 为服务全量替换的核心写入（UPSERT + 消失转 stale），
// 供单表事务与 HeartbeatFull 组合事务共用。
func replaceServicesTx(ctx context.Context, tx *sql.Tx, nodeID int64, svcs []ServiceRow, now int64) error {
	for i := range svcs {
		svcs[i].NodeID = nodeID
		svcs[i].UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `
INSERT INTO services(node_id, name, type, target, status, detail, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id, name) DO UPDATE SET
	type = excluded.type, target = excluded.target, status = excluded.status,
	detail = excluded.detail, updated_at = excluded.updated_at`,
			svcs[i].NodeID, svcs[i].Name, svcs[i].Type, svcs[i].Target,
			svcs[i].Status, svcs[i].Detail, svcs[i].UpdatedAt); err != nil {
			return err
		}
	}
	return markStale(ctx, tx, "services", nodeID, svcs, now)
}

// ReplaceNodeAgents 与 ReplaceNodeServices 同语义，作用于 ai_agents 表。
func (s *Store) ReplaceNodeAgents(ctx context.Context, nodeID int64, ags []AgentRow) (bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ?`, nodeID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	now := time.Now().Unix()
	if err := replaceAgentsTx(ctx, tx, nodeID, ags, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// replaceAgentsTx 为 agent 全量替换的核心写入，供单表事务与 HeartbeatFull 共用。
func replaceAgentsTx(ctx context.Context, tx *sql.Tx, nodeID int64, ags []AgentRow, now int64) error {
	for i := range ags {
		ags[i].NodeID = nodeID
		ags[i].UpdatedAt = now
		// invokable 由服务端强制为 false：发现 ≠ 可调用（DESIGN §4.2-B），
		// 客户端无任何途径写 true。
		ags[i].Invokable = false
		if _, err := tx.ExecContext(ctx, `
INSERT INTO ai_agents(node_id, name, type, version, path, status, invokable, detail, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id, name) DO UPDATE SET
	type = excluded.type, version = excluded.version, path = excluded.path,
	status = excluded.status, invokable = excluded.invokable,
	detail = excluded.detail, updated_at = excluded.updated_at`,
			ags[i].NodeID, ags[i].Name, ags[i].Type, ags[i].Version, ags[i].Path,
			ags[i].Status, ags[i].Invokable, ags[i].Detail, ags[i].UpdatedAt); err != nil {
			return err
		}
	}
	return markStale(ctx, tx, "ai_agents", nodeID, ags, now)
}

// markStale 将本次上报未出现的行 status 置 'stale'（全量替换语义的另一半）。
// table 仅为内部两处调用传入的字面量（"services"/"ai_agents"），非用户输入。
func markStale(ctx context.Context, tx *sql.Tx, table string, nodeID int64, rows any, now int64) error {
	var names []string
	switch v := rows.(type) {
	case []ServiceRow:
		names = make([]string, len(v))
		for i := range v {
			names[i] = v[i].Name
		}
	case []AgentRow:
		names = make([]string, len(v))
		for i := range v {
			names[i] = v[i].Name
		}
	default:
		return fmt.Errorf("markStale: unsupported row type")
	}
	var args []any
	var q string
	if len(names) == 0 {
		q = `UPDATE ` + table + ` SET status = 'stale', updated_at = ? WHERE node_id = ? AND status != 'stale'`
		args = []any{now, nodeID}
	} else {
		placeholders := strings.Repeat("?,", len(names))
		q = `UPDATE ` + table + ` SET status = 'stale', updated_at = ? WHERE node_id = ? AND status != 'stale' AND name NOT IN (` + placeholders[:len(placeholders)-1] + `)`
		args = make([]any, 0, len(names)+2)
		args = append(args, now, nodeID)
		for _, n := range names {
			args = append(args, n)
		}
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return nil
}

// ServiceRecord 为读出的 services 行（自检/测试与后续面板用）。
type ServiceRecord struct {
	NodeID    int64
	Name      string
	Type      string
	Target    string
	Status    string
	Detail    string
	UpdatedAt int64
}

// ListServices 按节点列出服务行（按名称排序）。
func (s *Store) ListServices(ctx context.Context, nodeID int64) ([]ServiceRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT node_id, name, type, target, status, detail, updated_at
FROM services WHERE node_id = ? ORDER BY name`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRecord
	for rows.Next() {
		var r ServiceRecord
		if err := rows.Scan(&r.NodeID, &r.Name, &r.Type, &r.Target, &r.Status, &r.Detail, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AgentRecord 为读出的 ai_agents 行。
type AgentRecord struct {
	NodeID    int64
	Name      string
	Type      string
	Version   string
	Path      string
	Status    string
	Invokable bool
	Detail    string
	UpdatedAt int64
}

// ListAgents 按节点列出 AI agent 行（按名称排序）。
func (s *Store) ListAgents(ctx context.Context, nodeID int64) ([]AgentRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT node_id, name, type, version, path, status, invokable, detail, updated_at
FROM ai_agents WHERE node_id = ? ORDER BY name`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRecord
	for rows.Next() {
		var r AgentRecord
		if err := rows.Scan(&r.NodeID, &r.Name, &r.Type, &r.Version, &r.Path, &r.Status, &r.Invokable, &r.Detail, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
