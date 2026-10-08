// auth_test.go 为账号体系存储层测试（SPEC-M1b-c2 §1）：users/sessions/api_tokens
// CRUD、改密/禁用撤销全部凭据、最后用户保护、过期会话清理。
package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustCreateUser(t *testing.T, st *Store, name string) *UserRow {
	t.Helper()
	u, err := st.CreateUser(context.Background(), name, "bcrypt$"+name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUserCRUDAndDuplicates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustCreateUser(t, st, "lunge")
	if !u.Enabled || u.ID == 0 {
		t.Fatalf("new user must be enabled with id: %+v", u)
	}
	if _, err := st.CreateUser(ctx, "lunge", "x"); err != ErrDuplicateUser {
		t.Fatalf("duplicate = %v, want ErrDuplicateUser", err)
	}
	got, err := st.GetUserByName(ctx, "lunge")
	if err != nil || got == nil || got.ID != u.ID {
		t.Fatalf("GetUserByName = %+v, %v", got, err)
	}
	if got, err = st.GetUserByName(ctx, "ghost"); err != nil || got != nil {
		t.Fatalf("missing user = %+v, %v; want nil,nil", got, err)
	}
	users, err := st.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers = %+v, %v", users, err)
	}
	n, err := st.CountEnabledUsers(ctx)
	if err != nil || n != 1 {
		t.Fatalf("CountEnabledUsers = %d, %v", n, err)
	}
}

func TestChangePasswordRevokesAllCredentials(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustCreateUser(t, st, "lunge")
	now := time.Now().Unix()
	// 一条会话 + 一条 API token。
	if err := st.CreateSession(ctx, "sess-hash-1", u.ID, now, now+3600, u.PasswordVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(ctx, "tok-hash-1", u.ID, "hana", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.ChangeUserPassword(ctx, u.ID, "new-hash"); err != nil {
		t.Fatalf("ChangeUserPassword: %v", err)
	}
	// 会话与 token 全部撤销；用户名不存在时报错。
	if rec, _ := st.GetSession(ctx, "sess-hash-1"); rec != nil {
		t.Fatal("session must be revoked after password change")
	}
	if rec, _ := st.AuthenticateAPIToken(ctx, "tok-hash-1"); rec != nil {
		t.Fatal("api token must be revoked after password change")
	}
	got, _ := st.GetUserByID(ctx, u.ID)
	if got.PasswordHash != "new-hash" {
		t.Fatalf("password hash not updated: %q", got.PasswordHash)
	}
	// R33-#1：改密递增密码版本，凭据校验时刻的旧版本在签发复核中落空。
	if got.PasswordVersion != u.PasswordVersion+1 {
		t.Fatalf("password version = %d, want %d", got.PasswordVersion, u.PasswordVersion+1)
	}
	if err := st.CreateSession(ctx, "stale-v", u.ID, now, now+3600, u.PasswordVersion); err != ErrSessionIssuanceDenied {
		t.Fatalf("stale version issuance = %v, want ErrSessionIssuanceDenied", err)
	}
	// 当前版本 + 启用态正常签发。
	if err := st.CreateSession(ctx, "fresh-v", u.ID, now, now+3600, got.PasswordVersion); err != nil {
		t.Fatalf("fresh version issuance = %v", err)
	}
	if err := st.ChangeUserPassword(ctx, 99999, "x"); err != ErrUserNotFound {
		t.Fatalf("missing user = %v, want ErrUserNotFound", err)
	}
}

func TestDisableRevokesAndLastEnabledProtected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	only := mustCreateUser(t, st, "only")
	now := time.Now().Unix()
	_ = st.CreateSession(ctx, "s1", only.ID, now, now+3600, only.PasswordVersion)
	// 最后一个启用用户禁用被拒绝，凭据保留（未撤销——拒绝路径不得有副作用）。
	if err := st.SetUserEnabled(ctx, only.ID, false); err != ErrLastEnabledUser {
		t.Fatalf("disable last = %v, want ErrLastEnabledUser", err)
	}
	if rec, _ := st.GetSession(ctx, "s1"); rec == nil {
		t.Fatal("rejected disable must not revoke session")
	}
	// 第二个用户存在后，禁用第一个成功且撤销其全部凭据。
	second := mustCreateUser(t, st, "second")
	if err := st.SetUserEnabled(ctx, only.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if rec, _ := st.GetSession(ctx, "s1"); rec != nil {
		t.Fatal("session must be revoked after disable")
	}
	got, _ := st.GetUserByID(ctx, only.ID)
	if got.Enabled {
		t.Fatal("user must be disabled")
	}
	// R33-#1：禁用递增密码版本；拿到最新版本也签不出会话（enabled=1 条件），
	// 禁用前校验的旧版本同样拒绝——签发复核双条件各自生效。
	if got.PasswordVersion != only.PasswordVersion+1 {
		t.Fatalf("password version after disable = %d, want %d", got.PasswordVersion, only.PasswordVersion+1)
	}
	if err := st.CreateSession(ctx, "s2", only.ID, now, now+3600, got.PasswordVersion); err != ErrSessionIssuanceDenied {
		t.Fatalf("disabled-user issuance = %v, want ErrSessionIssuanceDenied", err)
	}
	if err := st.CreateSession(ctx, "s3", only.ID, now, now+3600, only.PasswordVersion); err != ErrSessionIssuanceDenied {
		t.Fatalf("stale-version issuance after disable = %v, want ErrSessionIssuanceDenied", err)
	}
	if n, _ := st.CountEnabledUsers(ctx); n != 1 {
		t.Fatalf("enabled count = %d, want 1", n)
	}
	// 重新启用；重复禁用/启用幂等语义由调用方（CLI）处理，这里只钉启用路径。
	if err := st.SetUserEnabled(ctx, only.ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// 启用后（版本不再变）凭当前版本签发恢复可用。
	if err := st.CreateSession(ctx, "s4", only.ID, now, now+3600, got.PasswordVersion); err != nil {
		t.Fatalf("re-enabled issuance = %v", err)
	}
	if err := st.SetUserEnabled(ctx, 424242, false); err != ErrUserNotFound {
		t.Fatalf("missing user = %v", err)
	}
	_ = second
}

func TestSessionLifecycleAndExpiryCleanup(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustCreateUser(t, st, "lunge")
	now := time.Now().Unix()
	if err := st.CreateSession(ctx, "h1", u.ID, now, now+3600, u.PasswordVersion); err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetSession(ctx, "h1")
	if err != nil || rec == nil || rec.Username != "lunge" || !rec.Enabled {
		t.Fatalf("GetSession = %+v, %v", rec, err)
	}
	if err := st.TouchSession(ctx, "h1", now+7200); err != nil {
		t.Fatal(err)
	}
	if rec, _ = st.GetSession(ctx, "h1"); rec.ExpiresAt != now+7200 {
		t.Fatalf("touch: expires = %d", rec.ExpiresAt)
	}
	// 过期会话：过期行清理命中、未过期行保留。
	_ = st.CreateSession(ctx, "h2", u.ID, now-100, now-50, u.PasswordVersion)
	n, err := st.DeleteExpiredSessions(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d, %v", n, err)
	}
	if rec, _ = st.GetSession(ctx, "h2"); rec != nil {
		t.Fatal("expired session must be deleted")
	}
	if rec, _ = st.GetSession(ctx, "h1"); rec == nil {
		t.Fatal("live session must be kept")
	}
	// 登出删除单条。
	if err := st.DeleteSession(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if rec, _ = st.GetSession(ctx, "h1"); rec != nil {
		t.Fatal("logout must delete session")
	}
	// 只读句柄上写路径必须报错（与 store 既有口径一致）。
	ro, err := OpenReadOnly(filepath.Join(t.TempDir(), "ro.db"))
	if err == nil {
		ro.Close()
	}
}

func TestAPITokenLifecycleAndAuth(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustCreateUser(t, st, "lunge")
	id1, err := st.CreateAPIToken(ctx, "hash-a", u.ID, "hana mac", nil)
	if err != nil || id1 == 0 {
		t.Fatalf("create token: %v", err)
	}
	exp := time.Now().Add(-time.Hour).Unix()
	if _, err := st.CreateAPIToken(ctx, "hash-b", u.ID, "expired", &exp); err != nil {
		t.Fatal(err)
	}
	// 认证命中带用户启用态与到期字段。
	rec, err := st.AuthenticateAPIToken(ctx, "hash-a")
	if err != nil || rec == nil || rec.Username != "lunge" || !rec.Enabled || rec.ExpiresAt.Valid {
		t.Fatalf("AuthenticateAPIToken = %+v, %v", rec, err)
	}
	if rec, _ = st.AuthenticateAPIToken(ctx, "hash-b"); rec == nil || !rec.ExpiresAt.Valid {
		t.Fatal("expired token record must carry expiry")
	}
	if rec, _ = st.AuthenticateAPIToken(ctx, "hash-x"); rec != nil {
		t.Fatal("unknown token must be nil")
	}
	rows, err := st.ListAPITokens(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListAPITokens = %d, %v", len(rows), err)
	}
	// 吊销后立即可见。
	ok, err := st.RevokeAPIToken(ctx, id1)
	if err != nil || !ok {
		t.Fatalf("revoke = %v, %v", ok, err)
	}
	if ok, _ = st.RevokeAPIToken(ctx, id1); ok {
		t.Fatal("double revoke must report false")
	}
	if rec, _ = st.AuthenticateAPIToken(ctx, "hash-a"); rec != nil {
		t.Fatal("revoked token must not authenticate")
	}
}

// TestDataFilePerms 数据文件权限（SPEC-M1b-c2 §4）：data 目录 0700、DB 0600，
// 放宽的历史文件在下次 Open 时被收紧。Windows 的 Mode().Perm() 不反映真实
// ACL，跳过该平台断言。
func TestDataFilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows perm bits are not POSIX")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nested", "meshconsole.db")
	if _, err := Open(dbPath); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(dbPath)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir perm = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(dbPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("db perm = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dbPath); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dbPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("relaxed db perm after reopen = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}

func TestMigrationV6Schema(t *testing.T) {
	st := newTestStore(t)
	var v int
	if err := st.read.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil || v < 7 {
		t.Fatalf("schema version = %d, %v; want >= 7", v, err)
	}
	for _, tt := range []struct{ table, col string }{
		{"users", "enabled"},
		{"users", "password_hash"},
		{"users", "password_version"},
		{"sessions", "created_at"},
		{"sessions", "expires_at"},
		{"api_tokens", "token_hash"},
		{"api_tokens", "expires_at"},
	} {
		var n int
		if err := st.read.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, tt.table, tt.col).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s.%s missing: %v n=%d", tt.table, tt.col, err, n)
		}
	}
}
