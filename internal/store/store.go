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
	// TasksTruncated 为最近一次显式任务快照的截断标记（R27-#4，migration v5）。
	TasksTruncated bool
	// L2Allowed 为任务下发白名单标记（SPEC-M1d §1，migration v8）：由 console
	// 配置 l2_allowed_nodes 声明（mac-mini/windows true，云节点 false），
	// 启动与注册时同步入库；提交与领取双重复核均以此列为准。
	L2Allowed bool
	// Caps 为 agent 心跳上报的能力清单 JSON 数组（SPEC-M1d §5，migration v8；
	// 如 ["linux-systemd","linux-docker"]），下发时校验 kind 平台矩阵。
	Caps string
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

// dsnReadOnly 构造只读 DSN：mode=ro（文件层只读，库文件不存在时直接报错而不
// 创建）+ query_only=1（连接层再挡一道写路径，双保险）。不设 journal_mode 等
// 写语义 PRAGMA——只读连接不得改动库文件。
func dsnReadOnly(path string) string {
	v := url.Values{}
	v.Add("mode", "ro")
	v.Add("_pragma", "busy_timeout=5000")
	v.Add("_pragma", "query_only=1")
	return "file:" + path + "?" + v.Encode()
}

func openDB(path string, maxOpen int) (*sql.DB, error) {
	return openDBWithDsn(dsn(path), maxOpen)
}

func openDBWithDsn(dsn string, maxOpen int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
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
// 数据文件权限口径（SPEC-M1b-c2 §4）：data 目录 0700、DB/WAL/SHM 0600——
// 面板公网形态下这些文件含节点 token 哈希与口令哈希，全局可读等于泄露。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
		// MkdirAll 受 umask 影响且目录可能以更宽权限预先存在：显式收紧一次。
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("chmod db dir: %w", err)
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
	if err := tightenDataPerms(path); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// tightenDataPerms 收紧数据文件权限：DB 与 WAL/SHM 一律 0600（SPEC-M1b-c2 §4）。
// WAL/SHM 尚不存在时跳过——SQLite 新建它们时沿用主库文件的权限位，主库 0600
// 即传导；已存在的旧文件（历史版本以 0644 创建）就地收紧。
func tightenDataPerms(dbPath string) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s.write == nil {
		// 只读句柄（OpenReadOnly）：仅读池。
		return s.read.Close()
	}
	errW := s.write.Close()
	errR := s.read.Close()
	if errW != nil {
		return errW
	}
	return errR
}

// OpenReadOnly 以只读模式打开库（MCP/面板等只读消费方专用，SPEC-M1b-b §2）：
// mode=ro + query_only=1 双保险，库文件不存在时报错而不创建；不执行 migration
// （schema 由 console 服务模式负责）。返回的 Store 仅可调用读路径方法，
// 写句柄为 nil，任何写方法都会以错误暴露而非静默。
func OpenReadOnly(path string) (*Store, error) {
	read, err := openDBWithDsn(dsnReadOnly(path), 2)
	if err != nil {
		return nil, fmt.Errorf("open read-only db: %w", err)
	}
	return &Store{write: nil, read: read}, nil
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
	{
		// M1b-b（SPEC §3）：Headscale 集成落库。id 为 headscale 侧节点 id（外部
		// 主键，非自增）；每次拉取全量替换（事务内先清后插）。last_seen 为
		// headscale 报告的节点最近可见时刻（unix 秒，可空）；updated_at 为本库
		// 本次替换时刻——两者语义不同（拉取失败保留旧数据时不前移）。
		Version: 3,
		SQL: `
CREATE TABLE IF NOT EXISTS tailnet_nodes (
	id           INTEGER PRIMARY KEY,
	machine_name TEXT    NOT NULL,
	ips          TEXT    NOT NULL DEFAULT '',
	online       INTEGER NOT NULL DEFAULT 0,
	last_seen    INTEGER,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tailnet_nodes_updated ON tailnet_nodes(updated_at);
`,
	},
	{
		// M1b-c（SPEC §2.2）：agent 运行任务快照表 + AI agent 会话活跃度列。
		// agent_tasks 为「当前正在运行」的快照：心跳按节点全量替换（进程消失 =
		// 任务结束，行随之删除，不留历史）；历史任务档案属 M1c+，本期不做。
		// ai_agents 增列 last_activity（会话目录树最近 mtime，unix 秒，可空）与
		// session_files（目录树文件数，可空）——只 stat 不读内容的轻量辅证。
		Version: 4,
		SQL: `
CREATE TABLE IF NOT EXISTS agent_tasks (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	pid        INTEGER NOT NULL,
	agent_name TEXT    NOT NULL,
	cmd        TEXT    NOT NULL DEFAULT '',
	elapsed_s  INTEGER NOT NULL DEFAULT 0,
	cpu_pct    REAL,
	mem_pct    REAL,
	started_at INTEGER,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_node_updated ON agent_tasks(node_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_updated ON agent_tasks(updated_at);

	ALTER TABLE ai_agents ADD COLUMN last_activity INTEGER;
	ALTER TABLE ai_agents ADD COLUMN session_files INTEGER;
`,
	},
	{
		// m1b-c-fix（R27-#4）：节点级「任务清单截断」标记。单拍匹配任务数超过
		// 上限 64 时，agent 上报的 agent_tasks 是不完整子集——随最近一次显式
		// 任务快照写入该标记（快照字段缺席不改动），面板/MCP 据此标注「清单
		// 不完整」，截断可见不静默。
		Version: 5,
		SQL: `
ALTER TABLE nodes ADD COLUMN tasks_truncated INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		// M1b-c2（SPEC §1）：账号体系——users/sessions/api_tokens。会话与 API
		// token 库里只存 SHA-256 哈希；sessions.created_at 承载 30 天绝对期限
		// 基点（滑动续期不得越过）；api_tokens.token_hash 唯一索引即查表键。
		// 注：SPEC 原文称「migration v5」，v5 已被 m1b-c-fix 的 tasks_truncated
		// 占用（R27-#4），按既有序号顺延为 v6——结构以 SPEC 表定义为准。
		Version: 6,
		SQL: `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	enabled       INTEGER NOT NULL DEFAULT 1,
	created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS api_tokens (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	token_hash  TEXT    NOT NULL UNIQUE,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	description TEXT    NOT NULL DEFAULT '',
	expires_at  INTEGER,
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_id);
`,
	},
	{
		// m1b-c2-fix（R33-#1）：口令状态版本。会话签发在写入时刻按
		// 「password_version + enabled」复核——改密/禁用递增版本后，凭据校验与
		// 会话插入之间发生的撤销动作会使旧凭据的签发落空（无活会话可拿）。
		Version: 7,
		SQL: `
ALTER TABLE users ADD COLUMN password_version INTEGER NOT NULL DEFAULT 1;
`,
	},
	{
		// M1d（SPEC-M1d §1）：agent 任务下发通道。SPEC 原文称「migration v6」，
		// v6/v7 已被 M1b-c2 账号表与 R33-#1 占用，按既有序号顺延为 v8——结构以
		// SPEC 表定义为准。
		//   - commands：可靠执行协议核心表。command_id（uuid）为主键且永不复用；
		//     submission_key 唯一索引（幂等键，可空——键过期归档进 archived_key 后
		//     置 NULL 立即可复用，DESIGN §4.1-D）；archived 为生命周期属性列
		//     （非独立状态）；degraded 为 unknown/修正路径的降级历史标记；
		//     result_recorded 标记「console 已记录回执」（ack 判定依据——仅有
		//     结果的命令才可 ACK，sweep 产生的 unknown 无回执不得 ACK）。
		//   - nodes.l2_allowed：下发白名单（config 声明同步入库，双重复核用）；
		//     nodes.caps：agent 心跳上报的能力清单 JSON（下发前平台矩阵校验）。
		//   - api_tokens.scope：readonly|operator（M1b-c2 表加列；SPEC-M1d §1
		//     ——c2 批次未含该列，本批交付；旧 token 缺省 'readonly'）。
		Version: 8,
		SQL: `
CREATE TABLE IF NOT EXISTS commands (
	command_id             TEXT    PRIMARY KEY,
	node_id                INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	kind                   TEXT    NOT NULL,
	args_json              TEXT    NOT NULL DEFAULT '{}',
	status                 TEXT    NOT NULL DEFAULT 'pending',
	degraded               INTEGER NOT NULL DEFAULT 0,
	timeout_s              INTEGER NOT NULL DEFAULT 30,
	result_text            TEXT    NOT NULL DEFAULT '',
	exit_code              INTEGER,
	created_by             TEXT    NOT NULL DEFAULT '',
	created_by_token_scope TEXT    NOT NULL DEFAULT '',
	claimed_at             INTEGER,
	running_at             INTEGER,
	finished_at            INTEGER,
	submission_key         TEXT,
	archived_key           TEXT,
	archived               INTEGER NOT NULL DEFAULT 0,
	result_recorded        INTEGER NOT NULL DEFAULT 0,
	created_at             INTEGER NOT NULL,
	updated_at             INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_commands_submission_key
	ON commands(submission_key) WHERE submission_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_commands_node_status ON commands(node_id, status, archived);
CREATE INDEX IF NOT EXISTS idx_commands_status_claimed ON commands(status, claimed_at);
CREATE INDEX IF NOT EXISTS idx_commands_created ON commands(created_at);

ALTER TABLE nodes ADD COLUMN l2_allowed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN caps TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'readonly';
`,
	},
	{
		// M1d 修复轮（SPEC-M1d §3.5 会话活跃视图，伦哥 20:35 点名需求）。
		// SPEC 原文称「console 同批 migration 新表 agent_sessions」，v6-v8 已被
		// 账号表/口令版本/命令表占用，按既有序号顺延为 v9——结构以 SPEC 为准。
		//   - agent_sessions：活跃会话快照（agent 会话目录 mtime<30min 的会话
		//     文件，轻解析主题/当前动作），心跳按节点全量替换（同 agent_tasks
		//     口径：进程消失=会话结束，无 stale 簿记态）。(node_id, session_file)
		//     唯一：同节点同会话文件幂等。
		//   - agent_tasks.background：daemon 过滤标记（SPEC §3.5：elapsed > 1h
		//     的常驻进程不是「任务」——面板任务区不显示，数据保留入库）。
		Version: 9,
		SQL: `
CREATE TABLE IF NOT EXISTS agent_sessions (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id       INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	agent_name    TEXT    NOT NULL,
	session_file  TEXT    NOT NULL,
	started_at    INTEGER,
	last_activity INTEGER,
	topic         TEXT    NOT NULL DEFAULT '',
	recent_action TEXT    NOT NULL DEFAULT '',
	updated_at    INTEGER NOT NULL,
	UNIQUE(node_id, session_file)
);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_node_updated ON agent_sessions(node_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_updated ON agent_sessions(updated_at);

ALTER TABLE agent_tasks ADD COLUMN background INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		// M1d 修复轮 3（R41-B1 深修，伦哥定案：代次令牌贯穿全链）。
		// commands.lease_epoch 为租约代次令牌：首次领取置 1，租约过期回收重领
		// 自增（同 command_id 代次前移）。回执按 epoch 归属验证——旧代次迟到回执
		// 一律拒绝，agent 端对账文件按 (command_id, epoch) 命名，旧代次清理不再
		// 可能误删新代次已写入的结果（原 claimed_at 快照令牌的收口缺口）。
		Version: 10,
		SQL: `
ALTER TABLE commands ADD COLUMN lease_epoch INTEGER NOT NULL DEFAULT 0;
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
	if err := s.writable(); err != nil {
		return nil, err
	}
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

// nodeColumns 为 nodes 表认证/按名查询路径的列清单（scanNode 消费）。
const nodeColumns = `id, name, role, os, arch, tailnet_ip, public_ip, agent_version, status,
       token_hash, last_seen, last_success, created_at, tasks_truncated, l2_allowed, caps`

// AuthenticateNode 按节点 token 哈希取节点；未命中返回 nil（调用方一律回 401）。
func (s *Store) AuthenticateNode(ctx context.Context, tokenHash string) (*Node, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE token_hash = ?`, tokenHash)
	return scanNode(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanNode(row rowScanner) (*Node, error) {
	n := &Node{}
	var l2 int
	err := row.Scan(&n.ID, &n.Name, &n.Role, &n.OS, &n.Arch, &n.TailnetIP, &n.PublicIP,
		&n.AgentVersion, &n.Status, &n.TokenHash, &n.LastSeen, &n.LastSuccess, &n.CreatedAt,
		&n.TasksTruncated, &l2, &n.Caps)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n.L2Allowed = l2 == 1
	return n, nil
}

// Heartbeat 单事务完成一次基础心跳（节点时间戳 + 一条 metrics）。M1d 起新代码
// 一律走 HeartbeatFull（caps 承载）；本方法保留给测试与兼容旧调用方。
// 返回 false 表示节点不存在（token 失效）。
func (s *Store) Heartbeat(ctx context.Context, n *MetricsRow, agentVersion string) (bool, error) {
	if err := s.writable(); err != nil {
		return false, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ok, err := heartbeatTx(ctx, tx, n, agentVersion, time.Now().Unix(), nil)
	if err != nil || !ok {
		return ok, err
	}
	return true, tx.Commit()
}

// heartbeatTx 为心跳的核心写入（节点时间戳 + 一条 metrics [+ caps]），供单条
// 心跳事务与 HeartbeatFull 组合事务共用。返回 false 表示节点不存在。
// caps 非 nil 时随本拍覆盖 nodes.caps（SPEC-M1d §5 能力协商，空数组覆盖为 ”）；
// nil = 字段缺席，保留旧值（旧版本 agent 心跳不携带 caps）。
func heartbeatTx(ctx context.Context, tx *sql.Tx, n *MetricsRow, agentVersion string, now int64, caps *string) (bool, error) {
	successRefresh := "last_seen = ?"
	if n.CollectErrors == "" {
		successRefresh = "last_seen = ?, last_success = ?"
	}
	q := "UPDATE nodes SET agent_version = ?, status = 'online', " + successRefresh + " WHERE id = ?"
	args := []any{agentVersion}
	if caps != nil {
		q = "UPDATE nodes SET agent_version = ?, caps = ?, status = 'online', " + successRefresh + " WHERE id = ?"
		args = append(args, *caps)
	}
	if n.CollectErrors == "" {
		args = append(args, now, now)
	} else {
		args = append(args, now)
	}
	args = append(args, n.NodeID)
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
// services 全量替换（services 非 nil 时）+ agents 全量替换（agents 非 nil 时）+
// agent_tasks 全量替换（tasks 非 nil 时）+ agent_sessions 全量替换（sessions
// 非 nil 时，SPEC-M1d §3.5）+ caps 覆盖（caps 非 nil 时，SPEC-M1d §5）。
// 任何一步失败整体回滚——多次独立事务中途失败会留下「metrics 已落、清单未换」
// 的半轮数据，合并后与单条心跳同原子性。services/agents/tasks/sessions 传 nil
// 表示字段缺席（无变化不覆盖）。tasks 非 nil 时 tasksTruncated 随快照写入
// nodes.tasks_truncated（R27-#4：清单触顶截断可见，未截断的显式快照把标记复位
// 为 false；tasks 为 nil 时不改动标记）。caps 同语义：nil = 心跳未携带（旧版
// agent），保留旧值；非 nil（含空数组）= 覆盖。返回 false 表示节点不存在
// （token 失效）。
func (s *Store) HeartbeatFull(ctx context.Context, n *MetricsRow, agentVersion string, services *[]ServiceRow, agents *[]AgentRow, tasks *[]AgentTaskRow, tasksTruncated bool, sessions *[]AgentSessionRow, caps *string) (bool, error) {
	if err := s.writable(); err != nil {
		return false, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	ok, err := heartbeatTx(ctx, tx, n, agentVersion, now, caps)
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
	if tasks != nil {
		if err := replaceAgentTasksTx(ctx, tx, n.NodeID, *tasks, now); err != nil {
			return false, err
		}
		// 截断标记是快照的属性，与快照同事务落库（同生同灭，无半拍错位）。
		truncInt := 0
		if tasksTruncated {
			truncInt = 1
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET tasks_truncated = ? WHERE id = ?`, truncInt, n.NodeID); err != nil {
			return false, err
		}
	}
	if sessions != nil {
		if err := replaceAgentSessionsTx(ctx, tx, n.NodeID, *sessions, now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SweepOffline 将 last_seen 早于 cutoff 的在线节点标记为 offline，返回受影响行数。
func (s *Store) SweepOffline(ctx context.Context, cutoff int64) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
	res, err := s.write.ExecContext(ctx,
		`UPDATE nodes SET status = 'offline' WHERE status = 'online' AND last_seen < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupMetrics 删除 ts 早于 cutoff 的指标行，返回删除行数。
func (s *Store) CleanupMetrics(ctx context.Context, cutoff int64) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
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
	row := s.read.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE name = ?`, name)
	return scanNode(row)
}

// SetNodeLastSeen 直接改写节点 last_seen（运维调整/测试构造过期态）。
func (s *Store) SetNodeLastSeen(ctx context.Context, nodeID, unix int64) error {
	if err := s.writable(); err != nil {
		return err
	}
	_, err := s.write.ExecContext(ctx, `UPDATE nodes SET last_seen = ? WHERE id = ?`, unix, nodeID)
	return err
}

// SyncL2Allowed 把 console 配置声明的下发白名单（l2_allowed_nodes）同步进
// nodes.l2_allowed（SPEC-M1d §1：config 声明，库列为提交/领取双重复核的判定
// 依据）。名单内节点置 1、名单外全部置 0（配置为唯一事实源，重启/注册后同步）。
func (s *Store) SyncL2Allowed(ctx context.Context, allowedNames []string) error {
	if err := s.writable(); err != nil {
		return err
	}
	if _, err := s.write.ExecContext(ctx, `UPDATE nodes SET l2_allowed = 0 WHERE l2_allowed != 0`); err != nil {
		return err
	}
	for _, name := range allowedNames {
		if _, err := s.write.ExecContext(ctx, `UPDATE nodes SET l2_allowed = 1 WHERE name = ?`, name); err != nil {
			return err
		}
	}
	return nil
}

// DeleteNode 删除节点及其指标行（管理操作；节点 token 随行失效）。
func (s *Store) DeleteNode(ctx context.Context, nodeID int64) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
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
	// LastActivity/SessionFiles 为会话目录 stat 辅证（SPEC-M1b-c §2.1，
	// 只 stat 不读内容）；nil = 目录缺失或未产出（NULL）。
	LastActivity *int64
	SessionFiles *int64
	UpdatedAt    int64 // unix 秒，由 store 统一取 now
}

// ReplaceNodeServices 以本次上报为准全量替换某节点的服务清单（SPEC-M1b-a §4）：
// 上报条目逐条 UPSERT（同节点同名覆盖），本次未出现的历史条目 status 改写为
// 'stale'（保留行与历史，不删除）。单事务完成。
// 返回 false 表示节点不存在（心跳竞态中被删除），调用方按凭据失效处理。
func (s *Store) ReplaceNodeServices(ctx context.Context, nodeID int64, svcs []ServiceRow) (bool, error) {
	if err := s.writable(); err != nil {
		return false, err
	}
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
	if err := s.writable(); err != nil {
		return false, err
	}
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
		var lastAct, files any
		if ags[i].LastActivity != nil {
			lastAct = *ags[i].LastActivity
		}
		if ags[i].SessionFiles != nil {
			files = *ags[i].SessionFiles
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO ai_agents(node_id, name, type, version, path, status, invokable, detail, last_activity, session_files, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id, name) DO UPDATE SET
	type = excluded.type, version = excluded.version, path = excluded.path,
	status = excluded.status, invokable = excluded.invokable,
	detail = excluded.detail, last_activity = excluded.last_activity,
	session_files = excluded.session_files, updated_at = excluded.updated_at`,
			ags[i].NodeID, ags[i].Name, ags[i].Type, ags[i].Version, ags[i].Path,
			ags[i].Status, ags[i].Invokable, ags[i].Detail, lastAct, files, ags[i].UpdatedAt); err != nil {
			return err
		}
	}
	return markStale(ctx, tx, "ai_agents", nodeID, ags, now)
}

// AgentTaskRow 为 agent_tasks 表行（心跳上报的运行任务快照；按节点全量替换）。
type AgentTaskRow struct {
	NodeID    int64
	PID       int64
	AgentName string
	Cmd       string
	ElapsedS  int64
	CPUPct    *float64
	MemPct    *float64
	// StartedAt 为进程启动时刻（unix 秒，由 etime 反推）；nil = 未知（Windows
	// tasklist 兜底无此数据）。
	StartedAt sql.NullInt64
	// Background 标记常驻 daemon（SPEC-M1d §3.5：elapsed > 1h）——面板任务区
	// 不显示，数据保留（MCP 可见）。
	Background bool
	UpdatedAt  int64 // unix 秒，由 store 统一取 now
}

// replaceAgentTasksTx 为任务快照的核心写入（先清后插的全量替换，SPEC-M1b-c
// §2.2：进程消失 = 任务结束，不留历史行），供单表事务与 HeartbeatFull 共用。
func replaceAgentTasksTx(ctx context.Context, tx *sql.Tx, nodeID int64, tasks []AgentTaskRow, now int64) error {
	// 先清后插（与 tailnet_nodes 同口径）：快照表只反映「现在」，不存在 stale 簿记态。
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_tasks WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	for i := range tasks {
		tasks[i].NodeID = nodeID
		tasks[i].UpdatedAt = now
		var startedAt any
		if tasks[i].StartedAt.Valid {
			startedAt = tasks[i].StartedAt.Int64
		}
		bg := 0
		if tasks[i].Background {
			bg = 1
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO agent_tasks(node_id, pid, agent_name, cmd, elapsed_s, cpu_pct, mem_pct, started_at, background, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			tasks[i].NodeID, tasks[i].PID, tasks[i].AgentName, tasks[i].Cmd,
			tasks[i].ElapsedS, tasks[i].CPUPct, tasks[i].MemPct, startedAt, bg, tasks[i].UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

// AgentSessionRow 为 agent_sessions 表行（心跳上报的活跃会话快照；按节点全量
// 替换，SPEC-M1d §3.5）。
type AgentSessionRow struct {
	NodeID int64
	// AgentName / SessionFile 为会话归属（agent 名与会话文件路径）。
	AgentName   string
	SessionFile string
	// StartedAt 为会话开始时刻（文件创建时刻；nil = 平台不支持/未知）。
	StartedAt sql.NullInt64
	// LastActivity 为会话文件最近 mtime（活跃判定依据，恒有值）。
	LastActivity sql.NullInt64
	// Topic / RecentAction 为轻解析的会话主题与当前动作（各 ≤120 字；解析不
	// 出为空串——格式容错，显示空不报错）。
	Topic        string
	RecentAction string
	UpdatedAt    int64 // unix 秒，由 store 统一取 now
}

// replaceAgentSessionsTx 为活跃会话快照的核心写入（先清后插的全量替换，SPEC
// §3.5：会话文件静默=会话结束，无 stale 簿记态），供单表事务与 HeartbeatFull
// 共用。
func replaceAgentSessionsTx(ctx context.Context, tx *sql.Tx, nodeID int64, sessions []AgentSessionRow, now int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_sessions WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	for i := range sessions {
		sessions[i].NodeID = nodeID
		sessions[i].UpdatedAt = now
		var startedAt, lastAct any
		if sessions[i].StartedAt.Valid {
			startedAt = sessions[i].StartedAt.Int64
		}
		if sessions[i].LastActivity.Valid {
			lastAct = sessions[i].LastActivity.Int64
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO agent_sessions(node_id, agent_name, session_file, started_at, last_activity, topic, recent_action, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			sessions[i].NodeID, sessions[i].AgentName, sessions[i].SessionFile,
			startedAt, lastAct, sessions[i].Topic, sessions[i].RecentAction, sessions[i].UpdatedAt); err != nil {
			return err
		}
	}
	return nil
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
	// LastActivity/SessionFiles 为会话目录 stat 辅证（M1b-c）；NULL 保持 nil。
	LastActivity *int64
	SessionFiles *int64
	UpdatedAt    int64
}

func scanAgentRows(rows *sql.Rows) ([]AgentRecord, error) {
	defer rows.Close()
	var out []AgentRecord
	for rows.Next() {
		var r AgentRecord
		if err := rows.Scan(&r.NodeID, &r.Name, &r.Type, &r.Version, &r.Path, &r.Status,
			&r.Invokable, &r.Detail, &r.LastActivity, &r.SessionFiles, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAgents 按节点列出 AI agent 行（按名称排序）。
func (s *Store) ListAgents(ctx context.Context, nodeID int64) ([]AgentRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT node_id, name, type, version, path, status, invokable, detail,
       last_activity, session_files, updated_at
FROM ai_agents WHERE node_id = ? ORDER BY name`, nodeID)
	if err != nil {
		return nil, err
	}
	return scanAgentRows(rows)
}

// ---- M1b-b：tailnet（Headscale）与只读聚合查询 ----

// ErrReadOnly 只读句柄（OpenReadOnly）上禁止的写路径。
var ErrReadOnly = errors.New("store opened read-only; write path unavailable")

// writable 写路径守卫：OpenReadOnly 句柄上任何写方法直接报错而非 panic。
func (s *Store) writable() error {
	if s.write == nil {
		return ErrReadOnly
	}
	return nil
}

// TailnetNodeRow 为 tailnet_nodes 表行（Headscale 拉取结果，只读镜像）。
type TailnetNodeRow struct {
	ID          int64
	MachineName string
	// IPs 为该节点的 tailscale 地址（逗号连接，按 headscale 返回顺序）。
	IPs       string
	Online    bool
	LastSeen  sql.NullInt64 // unix 秒；headscale 未给出时 NULL
	UpdatedAt int64         // unix 秒，由 store 统一取 now
}

// ReplaceTailnetNodes 单事务全量替换 tailnet_nodes（SPEC-M1b-b §3：每次拉取
// 全量替换）。拉取失败时调用方不调用本方法（旧数据原样保留）。
func (s *Store) ReplaceTailnetNodes(ctx context.Context, nodes []TailnetNodeRow) error {
	if err := s.writable(); err != nil {
		return err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tailnet_nodes`); err != nil {
		return err
	}
	for _, n := range nodes {
		var lastSeen any
		if n.LastSeen.Valid {
			lastSeen = n.LastSeen.Int64
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO tailnet_nodes(id, machine_name, ips, online, last_seen, updated_at)
VALUES(?, ?, ?, ?, ?, ?)`,
			n.ID, n.MachineName, n.IPs, boolToInt(n.Online), lastSeen, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TailnetNodeRecord 为读出的 tailnet_nodes 行。
type TailnetNodeRecord struct {
	ID          int64
	MachineName string
	IPs         string
	Online      bool
	LastSeen    sql.NullInt64
	UpdatedAt   int64
}

// ListTailnetNodes 列出全部 tailnet 节点（按 id 排序）。
func (s *Store) ListTailnetNodes(ctx context.Context) ([]TailnetNodeRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT id, machine_name, ips, online, last_seen, updated_at
FROM tailnet_nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TailnetNodeRecord
	for rows.Next() {
		var r TailnetNodeRecord
		var online int
		if err := rows.Scan(&r.ID, &r.MachineName, &r.IPs, &online, &r.LastSeen, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Online = online == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAllServices 列出全网服务行（按 updated_at 倒序、名称次序稳定）。
func (s *Store) ListAllServices(ctx context.Context) ([]ServiceRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT node_id, name, type, target, status, detail, updated_at
FROM services ORDER BY name, node_id`)
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

// ListAllAgents 列出全网 AI agent 行。
func (s *Store) ListAllAgents(ctx context.Context) ([]AgentRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT node_id, name, type, version, path, status, invokable, detail,
       last_activity, session_files, updated_at
FROM ai_agents ORDER BY name, node_id`)
	if err != nil {
		return nil, err
	}
	return scanAgentRows(rows)
}

// AgentTaskRecord 为读出的 agent_tasks 行（当前运行任务快照）。
type AgentTaskRecord struct {
	NodeID    int64
	PID       int64
	AgentName string
	Cmd       string
	ElapsedS  int64
	CPUPct    sql.NullFloat64
	MemPct    sql.NullFloat64
	StartedAt sql.NullInt64
	// Background 为 daemon 过滤标记（SPEC-M1d §3.5：elapsed > 1h，面板任务区
	// 不显示、数据保留）。
	Background bool
	UpdatedAt  int64
}

// agentTaskColumns 为任务快照查询列清单（读侧统一，防列错位回归）。
const agentTaskColumns = `
	node_id, pid, agent_name, cmd, elapsed_s, cpu_pct, mem_pct, started_at,
	background, updated_at`

// ListAgentTasks 列出某节点当前任务快照（按 agent 名、pid 排序）。
func (s *Store) ListAgentTasks(ctx context.Context, nodeID int64) ([]AgentTaskRecord, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+agentTaskColumns+`
FROM agent_tasks WHERE node_id = ? ORDER BY agent_name, pid`, nodeID)
	if err != nil {
		return nil, err
	}
	return scanAgentTaskRows(rows)
}

// ListAllAgentTasks 列出全网当前任务快照（按节点、agent 名、pid 排序）。
func (s *Store) ListAllAgentTasks(ctx context.Context) ([]AgentTaskRecord, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+agentTaskColumns+`
FROM agent_tasks ORDER BY node_id, agent_name, pid`)
	if err != nil {
		return nil, err
	}
	return scanAgentTaskRows(rows)
}

func scanAgentTaskRows(rows *sql.Rows) ([]AgentTaskRecord, error) {
	defer rows.Close()
	var out []AgentTaskRecord
	for rows.Next() {
		var r AgentTaskRecord
		var bg int
		if err := rows.Scan(&r.NodeID, &r.PID, &r.AgentName, &r.Cmd, &r.ElapsedS,
			&r.CPUPct, &r.MemPct, &r.StartedAt, &bg, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Background = bg == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAgentTaskSnapshot 单只读事务取得全网任务快照与节点状态表（R31-#2 →
// R33-#7）：agent_tasks 与 nodes（status/tasks_truncated）取自同一 WAL 读
// 快照——并发心跳不再出现「旧任务清单配新截断标记/新状态」的错配，meshview
// 组装任务视图与截断标记并集共用这一份读取。
func (s *Store) ListAgentTaskSnapshot(ctx context.Context) ([]AgentTaskRecord, []NodeRecord, error) {
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	tasks, err := scanAgentTaskRowsFrom(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := scanNodeRecordsFrom(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	return tasks, nodes, tx.Commit()
}

// ListAgentSessionSnapshot 单只读事务取得全网活跃会话快照与节点状态表（与
// ListAgentTaskSnapshot 同款一致性口径：会话清单与节点名单同一 WAL 快照，
// 并发心跳/节点删除下不出现「会话行配不到节点名」的瞬态错配）。
func (s *Store) ListAgentSessionSnapshot(ctx context.Context) ([]AgentSessionRecord, []NodeRecord, error) {
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	rows, err := scanAgentSessionRowsFrom(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := scanNodeRecordsFrom(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	return rows, nodes, tx.Commit()
}

// scanAgentSessionRowsFrom 在 q（读库或只读事务）上取全网活跃会话快照。
func scanAgentSessionRowsFrom(ctx context.Context, q queryContext) ([]AgentSessionRecord, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+agentSessionColumns+`
FROM agent_sessions ORDER BY node_id, agent_name, session_file`)
	if err != nil {
		return nil, err
	}
	return scanAgentSessionRows(rows)
}

// scanAgentTaskRowsFrom 在 q（读库或只读事务）上取全网任务快照。
func scanAgentTaskRowsFrom(ctx context.Context, q queryContext) ([]AgentTaskRecord, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+agentTaskColumns+`
FROM agent_tasks ORDER BY node_id, agent_name, pid`)
	if err != nil {
		return nil, err
	}
	return scanAgentTaskRows(rows)
}

// AgentSessionRecord 为读出的 agent_sessions 行（活跃会话快照，SPEC-M1d §3.5）。
type AgentSessionRecord struct {
	NodeID       int64
	AgentName    string
	SessionFile  string
	StartedAt    sql.NullInt64
	LastActivity sql.NullInt64
	Topic        string
	RecentAction string
	UpdatedAt    int64
}

// agentSessionColumns 为会话快照查询列清单（读侧统一）。
const agentSessionColumns = `
	node_id, agent_name, session_file, started_at, last_activity, topic,
	recent_action, updated_at`

// ListAgentSessions 列出某节点当前活跃会话快照（按 agent 名、会话文件排序）。
func (s *Store) ListAgentSessions(ctx context.Context, nodeID int64) ([]AgentSessionRecord, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+agentSessionColumns+`
FROM agent_sessions WHERE node_id = ? ORDER BY agent_name, session_file`, nodeID)
	if err != nil {
		return nil, err
	}
	return scanAgentSessionRows(rows)
}

// ListAllAgentSessions 列出全网当前活跃会话快照（按节点、agent 名、会话文件排序）。
func (s *Store) ListAllAgentSessions(ctx context.Context) ([]AgentSessionRecord, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+agentSessionColumns+`
FROM agent_sessions ORDER BY node_id, agent_name, session_file`)
	if err != nil {
		return nil, err
	}
	return scanAgentSessionRows(rows)
}

func scanAgentSessionRows(rows *sql.Rows) ([]AgentSessionRecord, error) {
	defer rows.Close()
	var out []AgentSessionRecord
	for rows.Next() {
		var r AgentSessionRecord
		if err := rows.Scan(&r.NodeID, &r.AgentName, &r.SessionFile, &r.StartedAt,
			&r.LastActivity, &r.Topic, &r.RecentAction, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanNodeRecordsFrom 在 q（读库或只读事务）上取节点状态表（不含 token_hash）。
func scanNodeRecordsFrom(ctx context.Context, q queryContext) ([]NodeRecord, error) {
	rows, err := q.QueryContext(ctx, `
SELECT id, name, role, os, arch, tailnet_ip, public_ip, agent_version, status,
       last_seen, last_success, created_at, tasks_truncated, l2_allowed
FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRecord
	for rows.Next() {
		var r NodeRecord
		var l2 int
		if err := rows.Scan(&r.ID, &r.Name, &r.Role, &r.OS, &r.Arch, &r.TailnetIP,
			&r.PublicIP, &r.AgentVersion, &r.Status, &r.LastSeen, &r.LastSuccess, &r.CreatedAt,
			&r.TasksTruncated, &l2); err != nil {
			return nil, err
		}
		r.L2Allowed = l2 == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// queryContext 抽象 *sql.DB 与 *sql.Tx 共有的查询入口（同一事务内多查询用）。
type queryContext interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// MetricsStats 为某节点时间窗口内的指标摘要（MCP get_node 与面板共用）。
// 均值/峰值均为 NULL 时表示窗口内无该指标数据（不填 0）。
type MetricsStats struct {
	WindowSeconds int64
	Samples       int
	CPUAvg        sql.NullFloat64
	CPUMax        sql.NullFloat64
	// MemPctAvg/MemPctMax 基于窗口内有 mem_used 且 mem_total>0 的样本。
	MemPctAvg sql.NullFloat64
	MemPctMax sql.NullFloat64
	// DiskPctMax 为窗口内磁盘占用峰值（used/total，百分比）。
	DiskPctMax sql.NullFloat64
}

// MetricsStatsSince 汇总某节点 since（unix 秒）之后的指标。
// mem/disk total 为 0 的行不再被 WHERE 整行剔除（R19-#8）：占比分指标经
// NULLIF(total,0) 求值——除零样本只让对应 mem/disk 摘要保持 NULL，CPU 等
// 其余可用指标照常参与聚合（COUNT 为窗口内全部样本数）。
func (s *Store) MetricsStatsSince(ctx context.Context, nodeID int64, since int64) (*MetricsStats, error) {
	st := &MetricsStats{WindowSeconds: time.Now().Unix() - since}
	err := s.read.QueryRowContext(ctx, `
SELECT COUNT(*), AVG(cpu_pct), MAX(cpu_pct),
       AVG(mem_used * 100.0 / NULLIF(mem_total, 0)),
       MAX(mem_used * 100.0 / NULLIF(mem_total, 0)),
       MAX(disk_used * 100.0 / NULLIF(disk_total, 0))
FROM metrics
WHERE node_id = ? AND ts >= ?`, nodeID, since).Scan(
		&st.Samples, &st.CPUAvg, &st.CPUMax, &st.MemPctAvg, &st.MemPctMax, &st.DiskPctMax)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// MetricsPoint 为 sparkline 单点（cpu 与内存占比同一样本）。
type MetricsPoint struct {
	TS     int64
	CPUPct sql.NullFloat64
	MemPct sql.NullFloat64 // mem_total 缺失/为 0 的样本为 NULL
}

// MetricsSeriesSince 取某节点 since 之后、至多 limit 条样本（时间升序返回，
// 取的是最新 limit 条——超窗时丢弃最旧样本）。
func (s *Store) MetricsSeriesSince(ctx context.Context, nodeID int64, since int64, limit int) ([]MetricsPoint, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT ts, cpu_pct, mem_used * 100.0 / NULLIF(mem_total, 0)
FROM (SELECT ts, cpu_pct, mem_used, mem_total FROM metrics
      WHERE node_id = ? AND ts >= ? ORDER BY id DESC LIMIT ?)
ORDER BY ts`, nodeID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricsPoint
	for rows.Next() {
		var p MetricsPoint
		if err := rows.Scan(&p.TS, &p.CPUPct, &p.MemPct); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LatestMetricsTime 返回全库最新一条 metrics 的 ts（数据新鲜度口径）；
// 无数据返回 ok=false。
func (s *Store) LatestMetricsTime(ctx context.Context) (int64, bool, error) {
	var ts sql.NullInt64
	if err := s.read.QueryRowContext(ctx, `SELECT MAX(ts) FROM metrics`).Scan(&ts); err != nil {
		return 0, false, err
	}
	if !ts.Valid {
		return 0, false, nil
	}
	return ts.Int64, true, nil
}

// NodeRecord 为对外只读视图的节点行（不含 token_hash——只读消费方无需凭据材料）。
type NodeRecord struct {
	ID           int64
	Name         string
	Role         string
	OS           string
	Arch         string
	TailnetIP    string
	PublicIP     string
	AgentVersion string
	Status       string
	LastSeen     sql.NullInt64
	LastSuccess  sql.NullInt64
	CreatedAt    int64
	// TasksTruncated 为最近一次显式任务快照的截断标记（R27-#4，migration v5）。
	TasksTruncated bool
	// L2Allowed 为任务下发白名单标记（SPEC-M1d §1，migration v8）。
	L2Allowed bool
}

// ListNodes 列出全部节点（按名称排序；不含 token_hash）。
func (s *Store) ListNodes(ctx context.Context) ([]NodeRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT id, name, role, os, arch, tailnet_ip, public_ip, agent_version, status,
       last_seen, last_success, created_at, tasks_truncated, l2_allowed
FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRecord
	for rows.Next() {
		var r NodeRecord
		var l2 int
		if err := rows.Scan(&r.ID, &r.Name, &r.Role, &r.OS, &r.Arch, &r.TailnetIP,
			&r.PublicIP, &r.AgentVersion, &r.Status, &r.LastSeen, &r.LastSuccess, &r.CreatedAt,
			&r.TasksTruncated, &l2); err != nil {
			return nil, err
		}
		r.L2Allowed = l2 == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// NodeNameIDMap 返回 node_id → name 映射（meshview 组装视图用；只读）。
func (s *Store) NodeNameIDMap(ctx context.Context) (map[int64]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, name FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}
