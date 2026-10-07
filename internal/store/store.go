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
	now := time.Now().Unix()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

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
