// auth_test.go 为账号会话与登录防护测试（SPEC-M1b-c2 §1/§2）：
// 登录成功/失败、per-IP 限流、bcrypt 闸、dummy bcrypt 时序填充路径、
// 会话滑动/绝对期限、API token 认证、Cookie 属性。
package auth

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/store"
)

type nopLogger struct{}

func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nopLogger{}), st
}

func mustUser(t *testing.T, st *store.Store, name, password string) {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(context.Background(), name, hash); err != nil {
		t.Fatal(err)
	}
}

// TestLoginSuccessAndUnifiedFailure 登录成功签发 256bit token；未知用户/错密码
// 统一 ErrInvalidCredentials（不泄露区分）。
func TestLoginSuccessAndUnifiedFailure(t *testing.T) {
	m, st := newTestManager(t)
	mustUser(t, st, "lunge", "correct-horse-99")
	ctx := context.Background()
	tok, err := m.Login(ctx, "10.0.0.1", "lunge", "correct-horse-99")
	if err != nil || len(tok) != 64 {
		t.Fatalf("login = %q, %v", tok, err)
	}
	if _, err := m.Login(ctx, "10.0.0.2", "ghost", "whatever-123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user = %v", err)
	}
	if _, err := m.Login(ctx, "10.0.0.2", "lunge", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password = %v", err)
	}
}

// TestPerIPRateLimit 同一 IP 第 5 次失败后进入窗口（第 6 次被拒），其他 IP
// 不受影响；窗口内正确密码同样被拒（限先于验证）。
func TestPerIPRateLimit(t *testing.T) {
	m, st := newTestManager(t)
	mustUser(t, st, "lunge", "correct-horse-99")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := m.Login(ctx, "1.2.3.4", "lunge", "nope-nope-1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v", i, err)
		}
	}
	if _, err := m.Login(ctx, "1.2.3.4", "lunge", "correct-horse-99"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("6th attempt = %v, want ErrRateLimited", err)
	}
	// 其他 IP 照常登录。
	if _, err := m.Login(ctx, "5.6.7.8", "lunge", "correct-horse-99"); err != nil {
		t.Fatalf("other ip = %v", err)
	}
	// 未知用户名也消耗失败计数（dummy bcrypt 路径 + 计数），触顶同样 429。
	for i := 0; i < 5; i++ {
		_, _ = m.Login(ctx, "9.9.9.9", "ghost", "nope-nope-1")
	}
	if _, err := m.Login(ctx, "9.9.9.9", "ghost", "nope-nope-1"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("unknown user flood = %v, want ErrRateLimited", err)
	}
}

// TestIPWindowCap4096 失败窗口触顶 4096 后仍可记录（淘汰最旧 IP），不 panic
// 且 map 规模有界。
func TestIPWindowCap4096(t *testing.T) {
	m, _ := newTestManager(t)
	now := time.Now()
	for i := 0; i < maxTrackedIPs+64; i++ {
		m.recordFailure("10.1.0."+string(rune('a'+i%26))+string(rune('0'+i/26%10)), now)
	}
	m.mu.Lock()
	size := len(m.failWin)
	m.mu.Unlock()
	if size > maxTrackedIPs {
		t.Fatalf("window size = %d, cap %d", size, maxTrackedIPs)
	}
}

// TestBcryptGateAndWaitingCap 并发闸 ≤2 生效：占满后第三个排队等待释放；
// 排队人数触顶直接 429。
func TestBcryptGateAndWaitingCap(t *testing.T) {
	m, _ := newTestManager(t)
	// 占满两个闸位。
	m.acquireBcrypt(context.Background())
	m.acquireBcrypt(context.Background())
	released := make(chan struct{})
	go func() {
		// 第三个在闸满时排队；100ms 后释放一个闸位，排队者应当进入。
		time.Sleep(100 * time.Millisecond)
		m.releaseBcrypt()
		close(released)
	}()
	done := make(chan error, 1)
	go func() { done <- m.acquireBcrypt(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("queued acquire = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued acquire never entered after release")
	}
	<-released
	m.releaseBcrypt()
	m.releaseBcrypt()
	// 排队人数触顶且闸满：直接 429，不阻塞（闸未满时走快速路径不检查排队数）。
	m.acquireBcrypt(context.Background())
	m.acquireBcrypt(context.Background())
	m.waiting.Store(maxBcryptWaiters)
	if err := m.acquireBcrypt(context.Background()); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("saturated queue = %v, want ErrRateLimited", err)
	}
	m.waiting.Store(0)
	m.releaseBcrypt()
	m.releaseBcrypt()
}

// TestSessionLifecycle 会话校验：有效放行；登出后失效；过期失效；禁用用户
// 会话失效；绝对期限（created_at 满 30 天）不可续越。
func TestSessionLifecycle(t *testing.T) {
	m, st := newTestManager(t)
	mustUser(t, st, "lunge", "correct-horse-99")
	ctx := context.Background()
	tok, err := m.Login(ctx, "10.0.0.1", "lunge", "correct-horse-99")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.CheckSession(ctx, tok)
	if err != nil || sess.Username != "lunge" {
		t.Fatalf("CheckSession = %+v, %v", sess, err)
	}
	// 登出后失效。
	if err := m.Logout(ctx, sess.TokenHash); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CheckSession(ctx, tok); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("after logout = %v", err)
	}
	// 滑动期限过期。
	now := time.Now().Unix()
	u, _ := st.GetUserByName(ctx, "lunge")
	_ = st.CreateSession(ctx, hashToken("t2"), u.ID, now, now-1, u.PasswordVersion)
	if _, err := m.CheckSession(ctx, "t2"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expired = %v", err)
	}
	// 绝对期限：created_at 在 31 天前、滑动期限仍在未来 → 一律失效（续期不越过）。
	_ = st.CreateSession(ctx, hashToken("t3"), u.ID, now-int64((30*24+24)*time.Hour/time.Second), now+3600, u.PasswordVersion)
	if _, err := m.CheckSession(ctx, "t3"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("absolute cap = %v", err)
	}
	// 禁用用户的会话失效（disable 后 GetSession Enabled=false）。
	tok2, _ := m.Login(ctx, "10.0.0.1", "lunge", "correct-horse-99")
	if err := st.SetUserEnabled(ctx, u.ID, false); err != nil {
		// 只有一个用户 → 最后用户保护拒绝；再造一个用户后禁用。
		mustUser(t, st, "second", "another-pass-99")
		if err := st.SetUserEnabled(ctx, u.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.CheckSession(ctx, tok2); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("disabled user session = %v", err)
	}
}

// TestAPITokenAuth API token 认证：有效命中、无 token/错 token/过期/禁用用户
// 全部 ErrUnauthorized。
func TestAPITokenAuth(t *testing.T) {
	m, st := newTestManager(t)
	mustUser(t, st, "lunge", "correct-horse-99")
	ctx := context.Background()
	plain, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != len(APITokenPrefix)+64 {
		t.Fatalf("token len = %d", len(plain))
	}
	if _, err := st.CreateAPIToken(ctx, hashToken(plain), 1, "hana", nil); err != nil {
		t.Fatal(err)
	}
	rec, err := m.CheckAPIToken(ctx, plain)
	if err != nil || rec.Username != "lunge" {
		t.Fatalf("CheckAPIToken = %+v, %v", rec, err)
	}
	for _, bad := range []string{"", "wrong-token", plain + "x"} {
		if _, err := m.CheckAPIToken(ctx, bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("bad token %q = %v", bad, err)
		}
	}
	// 过期 token。
	exp := time.Now().Add(-time.Hour)
	expired, _ := NewAPIToken()
	ev := exp.Unix()
	_, _ = st.CreateAPIToken(ctx, hashToken(expired), 1, "old", &ev)
	if _, err := m.CheckAPIToken(ctx, expired); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired = %v", err)
	}
}

// TestHashPassword 口令哈希：bcrypt cost 10 可验证；短口令拒绝。
func TestHashPassword(t *testing.T) {
	h, err := HashPassword("good-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) < 59 || h[:4] != "$2a$" { // bcrypt 前缀
		t.Fatalf("hash = %q", h)
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password must be rejected")
	}
}

// TestSessionCleanupLoop 过期会话被周期清理（注入极短间隔跑两拍）。
func TestSessionCleanupLoop(t *testing.T) {
	_, st := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mustUser(t, st, "lunge", "correct-horse-99")
	u, _ := st.GetUserByName(ctx, "lunge")
	now := time.Now().Unix()
	_ = st.CreateSession(ctx, hashToken("gone"), u.ID, now-100, now-50, u.PasswordVersion)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); SessionCleanupLoop(ctx, st, nopLogger{}, 10*time.Millisecond) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec, _ := st.GetSession(ctx, hashToken("gone")); rec == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expired session never cleaned")
}

// TestLoginRacePasswordChange 改密与登录并发交错（R33-#1，模拟事务间隙）：
// 登录已读旧哈希、bcrypt 校验通过之后，改密事务在签发前抢先完成——签发复核
// 必须整体拒绝，旧凭据拿不到活会话；新凭据照常登录（复核只拦旧凭据）。
func TestLoginRacePasswordChange(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()
	mustUser(t, st, "lunge", "old-password-99")
	u, err := st.GetUserByName(ctx, "lunge")
	if err != nil || u == nil {
		t.Fatal(err)
	}
	newHash, err := HashPassword("new-password-99")
	if err != nil {
		t.Fatal(err)
	}
	m.afterValidate = func() {
		// 此刻登录已读过旧哈希且校验通过：改密事务在「校验后、签发前」间隙提交，
		// 撤销全部既有凭据——修复前登录随后仍会插入有效会话（缺陷本体）。
		if err := st.ChangeUserPassword(ctx, u.ID, newHash); err != nil {
			t.Errorf("change password in the gap: %v", err)
		}
	}
	tok, err := m.Login(ctx, "10.0.0.1", "lunge", "old-password-99")
	if !errors.Is(err, ErrInvalidCredentials) || tok != "" {
		t.Fatalf("stale-credential login = (%q, %v), want ErrInvalidCredentials + empty token", tok, err)
	}
	// 被拒签发不得留下任何会话行。
	if rec, _ := st.GetSession(ctx, hashToken(tok)); tok != "" && rec != nil {
		t.Fatal("denied issuance must not create a session row")
	}
	// 新凭据照常登录并拿到活会话。
	m.afterValidate = nil
	tok2, err := m.Login(ctx, "10.0.0.2", "lunge", "new-password-99")
	if err != nil {
		t.Fatalf("login with new password = %v", err)
	}
	if sess, err := m.CheckSession(ctx, tok2); err != nil || sess.Username != "lunge" {
		t.Fatalf("new session = %+v, %v", sess, err)
	}
}

// TestLoginRaceDisable 禁用与登录并发交错（R33-#1 同口径）：凭据校验后账号
// 在签发前被禁用，旧凭据签发被拒、无会话行落库。
func TestLoginRaceDisable(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()
	mustUser(t, st, "lunge", "correct-horse-99")
	mustUser(t, st, "second", "another-pass-99") // 保底：绕开最后启用用户保护
	u, err := st.GetUserByName(ctx, "lunge")
	if err != nil || u == nil {
		t.Fatal(err)
	}
	m.afterValidate = func() {
		if err := st.SetUserEnabled(ctx, u.ID, false); err != nil {
			t.Errorf("disable in the gap: %v", err)
		}
	}
	tok, err := m.Login(ctx, "10.0.0.1", "lunge", "correct-horse-99")
	if !errors.Is(err, ErrInvalidCredentials) || tok != "" {
		t.Fatalf("disabled-in-gap login = (%q, %v), want ErrInvalidCredentials + empty token", tok, err)
	}
	if rec, _ := st.GetSession(ctx, hashToken(tok)); tok != "" && rec != nil {
		t.Fatal("denied issuance must not create a session row")
	}
}
