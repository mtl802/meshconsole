// auth.go 为账号体系的存储层（SPEC-M1b-c2 §1）：users/sessions/api_tokens 三表
// 的读写。凭据材料口径：
//   - 口令只存 bcrypt 哈希（cost 10 由调用方 auth 包保证，本层不二次哈希）；
//   - 会话与 API token 只存 SHA-256 hex，明文仅在签发响应/CLI 输出出现一次；
//   - 改密与禁用用户在同一事务内撤销该用户全部 session 与 api_tokens
//     （SPEC §1：改密/disable 用户 → 撤销该用户全部 session 与 api_tokens）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrDuplicateUser 用户名已存在（创建防护）。
var ErrDuplicateUser = errors.New("username already exists")

// ErrLastEnabledUser 最后一个启用用户不可禁用（SPEC §1：disable 最后一个启用
// 用户时拒绝并提示——否则公网形态下无人可登录，等于把自己锁在门外）。
var ErrLastEnabledUser = errors.New("cannot disable the last enabled user")

// ErrUserNotFound 用户不存在。
var ErrUserNotFound = errors.New("user not found")

// ErrSessionIssuanceDenied 会话签发时的用户状态复核失败（R33-#1）：凭据校验
// 之后、会话插入之前，该用户的密码已被修改或账号已被禁用（改密/禁用事务在
// 间隙中先完成）——签发整体拒绝，旧凭据不得换来活会话。
var ErrSessionIssuanceDenied = errors.New("user state changed since credential check; session issuance denied")

// API token scope 枚举（SPEC-M1d §1/§4）：readonly=既有只读工具；operator=
// 额外开放 submit_command/get_command/list_commands。旧 token 列缺省 readonly。
const (
	ScopeReadonly = "readonly"
	ScopeOperator = "operator"
)

// UserRow 为 users 表行。
type UserRow struct {
	ID           int64
	Username     string
	PasswordHash string
	Enabled      bool
	CreatedAt    int64 // unix 秒
	// PasswordVersion 为口令状态版本（migration v7）：CreateUser 起始为 1，
	// 每次改密与禁用递增。会话签发按签发事务执行时刻的当前版本复核（R33-#1）。
	PasswordVersion int64
}

// SessionRecord 为 sessions 表行（联查 users 的用户名与启用态——认证时必须在
// 同一读取内确认用户仍启用，禁用用户不得凭未过期会话继续访问）。
type SessionRecord struct {
	TokenHash string
	UserID    int64
	Username  string
	Enabled   bool
	CreatedAt int64 // unix 秒；30 天绝对期限基点
	ExpiresAt int64 // unix 秒；滑动续期目标（不超过 CreatedAt+30d）
}

// APITokenRecord 为 api_tokens 表行（联查用户名供 CLI list 展示）。TokenHash
// 仅内部比对用，CLI 输出不得回显。
type APITokenRecord struct {
	ID          int64
	TokenHash   string
	UserID      int64
	Username    string
	Description string
	Scope       string        // readonly|operator（SPEC-M1d §1，migration v8）
	ExpiresAt   sql.NullInt64 // unix 秒；NULL = 长期有效
	CreatedAt   int64
}

// APITokenAuth 为 MCP Bearer 认证的查表结果（仅认证所需字段）。
type APITokenAuth struct {
	TokenID   int64
	UserID    int64
	Username  string
	Enabled   bool
	ExpiresAt sql.NullInt64
	TokenHash string // 认证命中后应用层 constant-time 复核用
	Scope     string // readonly|operator（submit_command 等写路径授权依据）
	CreatedAt int64
}

// CreateUser 插入用户（口令哈希由调用方经 bcrypt 生成）。用户名唯一。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*UserRow, error) {
	if err := s.writable(); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, enabled, created_at) VALUES(?, ?, 1, ?)`,
		username, passwordHash, now)
	if err != nil {
		// UNIQUE 冲突 → 用户名已存在（modernc/sqlite 报 SQLITE_CONSTRAINT）。
		if isUniqueViolation(err) {
			return nil, ErrDuplicateUser
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	// PasswordVersion=1 与 migration v7 的列缺省一致（INSERT 未携带该列）。
	return &UserRow{ID: id, Username: username, PasswordHash: passwordHash, Enabled: true,
		CreatedAt: now, PasswordVersion: 1}, nil
}

// isUniqueViolation 报告 err 是否为 SQLite 唯一约束冲突（驱动错误文本以
// "UNIQUE constraint failed" 为稳定前缀特征）。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ListUsers 列出全部用户（按用户名排序）。
func (s *Store) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, username, password_hash, enabled, created_at, password_version FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var u UserRow
		var enabled int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &enabled, &u.CreatedAt, &u.PasswordVersion); err != nil {
			return nil, err
		}
		u.Enabled = enabled == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUserByName 按用户名取用户；不存在返回 nil, nil。
func (s *Store) GetUserByName(ctx context.Context, username string) (*UserRow, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT id, username, password_hash, enabled, created_at, password_version FROM users WHERE username = ?`, username)
	return scanUser(row)
}

// GetUserByID 按主键取用户；不存在返回 nil, nil。
func (s *Store) GetUserByID(ctx context.Context, id int64) (*UserRow, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT id, username, password_hash, enabled, created_at, password_version FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func scanUser(row rowScanner) (*UserRow, error) {
	var u UserRow
	var enabled int
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &enabled, &u.CreatedAt, &u.PasswordVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled == 1
	return &u, nil
}

// CountEnabledUsers 返回启用用户数（公网启动门槛与最后用户保护共用）。
func (s *Store) CountEnabledUsers(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE enabled = 1`).Scan(&n)
	return n, err
}

// ChangeUserPassword 改口令并撤销该用户全部 session 与 api_tokens（SPEC §1）。
// 单事务：任何一步失败整体回滚，不出现「改了密、旧 token 还活着」的窗口。
// 同时递增 password_version（R33-#1）：凭据校验后、会话插入前发生的改密，
// 会被签发时的版本复核拒绝——旧凭据在间隙中换不来活会话。
func (s *Store) ChangeUserPassword(ctx context.Context, userID int64, passwordHash string) error {
	if err := s.writable(); err != nil {
		return err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_version = password_version + 1 WHERE id = ?`, passwordHash, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	if err := revokeUserCredentialsTx(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetUserEnabled 启用/禁用用户；禁用时先做最后用户保护（排除自身后无启用用户
// 即拒绝），并在同一事务内撤销该用户全部 session 与 api_tokens。禁用同时递增
// password_version（R33-#1）：与改密同口径，登录间隙发生的禁用会被签发复核拒绝。
func (s *Store) SetUserEnabled(ctx context.Context, userID int64, enabled bool) error {
	if err := s.writable(); err != nil {
		return err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := `UPDATE users SET enabled = ? WHERE id = ?`
	if !enabled {
		q = `UPDATE users SET enabled = ?, password_version = password_version + 1 WHERE id = ?`
	}
	res, err := tx.ExecContext(ctx, q, boolToInt(enabled), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	if !enabled {
		var others int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE enabled = 1 AND id != ?`, userID).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrLastEnabledUser
		}
		if err := revokeUserCredentialsTx(ctx, tx, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// revokeUserCredentialsTx 撤销某用户全部会话与 API token（事务内）。
func revokeUserCredentialsTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID)
	return err
}

// CreateSession 登记会话（token 哈希为主键）。createdAt/expiresAt 由调用方
// （auth 包）按滑动窗口与 30 天绝对期限计算。passwordVersion 为登录凭据校验
// 时读到的用户版本——签发以单条 INSERT…SELECT 在写入时刻复核（R33-#1）：
// 「WHERE id=? AND password_version=? AND enabled=1」不满足（凭据校验后用户
// 已改密/被禁用/已删除）即 0 行受影响、整体拒绝签发（ErrSessionIssuanceDenied），
// 条件求值与会话插入同为一条语句原子完成，不存在可插入的中间窗口。
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID, createdAt, expiresAt, passwordVersion int64) error {
	if err := s.writable(); err != nil {
		return err
	}
	res, err := s.write.ExecContext(ctx, `
INSERT INTO sessions(token_hash, user_id, created_at, expires_at)
SELECT ?, ?, ?, ? FROM users WHERE id = ? AND password_version = ? AND enabled = 1`,
		tokenHash, userID, createdAt, expiresAt, userID, passwordVersion)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrSessionIssuanceDenied
	}
	return nil
}

// GetSession 按 token 哈希取会话（联查用户名与启用态）；不存在返回 nil, nil。
func (s *Store) GetSession(ctx context.Context, tokenHash string) (*SessionRecord, error) {
	row := s.read.QueryRowContext(ctx, `
SELECT s.token_hash, s.user_id, u.username, u.enabled, s.created_at, s.expires_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ?`, tokenHash)
	var r SessionRecord
	var enabled int
	err := row.Scan(&r.TokenHash, &r.UserID, &r.Username, &enabled, &r.CreatedAt, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled == 1
	return &r, nil
}

// TouchSession 滑动续期：改写会话 expires_at（绝对期限上限由调用方计算）。
func (s *Store) TouchSession(ctx context.Context, tokenHash string, expiresAt int64) error {
	if err := s.writable(); err != nil {
		return err
	}
	_, err := s.write.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, expiresAt, tokenHash)
	return err
}

// DeleteSession 注销单个会话（登出）。
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if err := s.writable(); err != nil {
		return err
	}
	_, err := s.write.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteExpiredSessions 清理过期会话行（SweepLoop 周期调用），返回删除行数。
func (s *Store) DeleteExpiredSessions(ctx context.Context, now int64) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
	res, err := s.write.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CreateAPIToken 签发 API token 记录（expiresAt nil = 长期有效），返回主键 id
// 供 CLI revoke 使用。scope ∈ {readonly, operator}（SPEC-M1d §4：operator 才能调
// submit_command；缺省 readonly 由调用方保证——旧 token 列缺省亦为 readonly）。
func (s *Store) CreateAPIToken(ctx context.Context, tokenHash string, userID int64, description string, expiresAt *int64, scope string) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	var exp any
	if expiresAt != nil {
		exp = *expiresAt
	}
	res, err := s.write.ExecContext(ctx, `
INSERT INTO api_tokens(token_hash, user_id, description, expires_at, created_at, scope)
VALUES(?, ?, ?, ?, ?, ?)`, tokenHash, userID, description, exp, now, scope)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAPITokens 列出全部 API token（联查用户名，按创建时间倒序）。
func (s *Store) ListAPITokens(ctx context.Context) ([]APITokenRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT t.id, t.token_hash, t.user_id, u.username, t.description, t.scope, t.expires_at, t.created_at
FROM api_tokens t JOIN users u ON u.id = t.user_id
ORDER BY t.created_at DESC, t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APITokenRecord
	for rows.Next() {
		var r APITokenRecord
		if err := rows.Scan(&r.ID, &r.TokenHash, &r.UserID, &r.Username, &r.Description, &r.Scope, &r.ExpiresAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeAPIToken 按主键吊销 API token；返回 false 表示 id 不存在。
func (s *Store) RevokeAPIToken(ctx context.Context, id int64) (bool, error) {
	if err := s.writable(); err != nil {
		return false, err
	}
	res, err := s.write.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AuthenticateAPIToken 按哈希查 API token（联查用户启用态）；未命中返回 nil, nil。
// 到期校验由调用方完成（本层不取 now，保持纯查表语义）。
func (s *Store) AuthenticateAPIToken(ctx context.Context, tokenHash string) (*APITokenAuth, error) {
	row := s.read.QueryRowContext(ctx, `
SELECT t.id, t.user_id, u.username, u.enabled, t.expires_at, t.token_hash, t.scope, t.created_at
FROM api_tokens t JOIN users u ON u.id = t.user_id
WHERE t.token_hash = ?`, tokenHash)
	var a APITokenAuth
	var enabled int
	err := row.Scan(&a.TokenID, &a.UserID, &a.Username, &enabled, &a.ExpiresAt, &a.TokenHash, &a.Scope, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	return &a, nil
}
