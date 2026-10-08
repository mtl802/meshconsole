// Package auth 实现面板账号会话与 MCP API token 认证（SPEC-M1b-c2 §1/§2）。
//
// 安全口径：
//   - 认证在所有网络形态始终开启（含回环 listen），不设「内网免登录」路径；
//   - 登录防护三件套：per-IP 5 次失败/分 → 429（滑动窗，IP 条目上限 4096）、
//     全局并发 bcrypt ≤2（超出排队，等待上限后 429）、未知用户名也执行 dummy
//     bcrypt（防时序枚举）；
//   - 会话 token 256bit，Cookie mc_session（Path=/、host-only 不设 Domain、
//     HttpOnly、Secure、SameSite=Lax）；每次认证请求滑动续期，绝对期限 30 天
//     （续期不越过）；改密/disable 用户 → 撤销该用户全部 session 与 api_tokens
//     （撤销逻辑在 store 层同事务完成，本层只触发）；
//   - 会话签发复核（R33-#1）：插入会话时校验用户 password_version 与启用态
//     仍与凭据校验时一致，改密/禁用在「校验后、签发前」的间隙完成也照样拒绝
//     ——旧凭据任何时刻都换不来活会话；
//   - 登录失败信息统一「用户名或密码错误」，不区分未知用户/错密码/禁用用户
//     （禁用用户的会话已在 disable 时全部撤销，这里仅兜底）。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mtl802/meshconsole/internal/store"
)

const (
	// SessionCookieName 为面板会话 Cookie 名（SPEC §1 指定 mc_session）。
	SessionCookieName = "mc_session"
	// bcryptCost 为口令哈希成本（SPEC §1 指定 cost 10）。
	bcryptCost = 10
	// sessionTTL 为滑动续期窗口：每次认证请求把 expires_at 推到 now+sessionTTL。
	sessionTTL = 7 * 24 * time.Hour
	// sessionAbsolute 为会话绝对期限（SPEC §1：30 天，续期不越过）。
	sessionAbsolute = 30 * 24 * time.Hour
	// renewStep 为续期节流：距上次续期不足该窗口时不写库（面板 15s 轮询，
	// 不节流会每 15s 产生一次写事务挤占单写连接）。
	renewStep = time.Hour
	// loginFailuresPerMin 为单 IP 每分钟登录失败上限（SPEC §1：5 次/分）。
	loginFailuresPerMin = 5
	// loginWindow 为失败计数滑动窗宽。
	loginWindow = time.Minute
	// maxTrackedIPs 为失败窗口的 IP 条目上限（SPEC §1：滑动窗 cap 4096）。
	maxTrackedIPs = 4096
	// maxBcryptConcurrent 为全局 bcrypt 并发上限（SPEC §1：≤2）。
	maxBcryptConcurrent = 2
	// bcryptQueueWait 为 bcrypt 满载时的排队等待上限，超时 429。
	bcryptQueueWait = 5 * time.Second
	// maxBcryptWaiters 为排队人数上限：满载且排队 ≥ 该值直接 429，
	// 防分布式慢速攻击把 goroutine 堆积成内存面。
	maxBcryptWaiters = 32
	// minPasswordLen 为口令最小长度（`user add`/`passwd` CLI 侧同样校验）。
	minPasswordLen = 8
	// APITokenPrefix 为 API token 明文前缀（标识用途，泄露可识别来源）。
	APITokenPrefix = "mcp_"
)

// 登录与认证错误（错误文本即用户可见文案，SPEC §3 中文化口径）。
var (
	// ErrInvalidCredentials 统一登录失败：未知用户/错密码/禁用用户不区分。
	ErrInvalidCredentials = errors.New("用户名或密码错误")
	// ErrRateLimited 登录尝试过频（per-IP 窗口或 bcrypt 排队超限）。
	ErrRateLimited = errors.New("尝试过于频繁，请稍后再试")
	// ErrUnauthorized 供 API 类端点的认证失败（Bearer token 缺失/无效/过期）。
	ErrUnauthorized = errors.New("认证失败：缺少或无效的 Bearer token")
	// ErrSessionExpired 会话过期/被撤销（登出、改密、禁用）。
	ErrSessionExpired = errors.New("会话已失效，请重新登录")
)

// Manager 持有账号会话与登录防护状态。
type Manager struct {
	st  *store.Store
	log logger

	// bcryptGate 为全局 bcrypt 并发闸（容量 2）；waiting 计数排队人数。
	bcryptGate chan struct{}
	waiting    atomic.Int64

	// dummyHash 为未知用户名的时序填充哈希（惰性生成一次，非机密材料）。
	dummyOnce sync.Once
	dummyHash []byte
	dummyErr  error

	mu      sync.Mutex
	failWin map[string][]time.Time // IP → 窗口内失败时刻

	// afterValidate 为凭据校验通过后、会话签发前的钩子（R33-#1 并发交错单测的
	// 事务间隙注入点；生产恒为 nil，仅一个 nil 判断的开销）。
	afterValidate func()
}

// logger 为 slog *slog.Logger 的最小接口（避免包间 import 耦合测试桩）。
type logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Info(msg string, args ...any)
}

// New 构造 Manager。
func New(st *store.Store, log logger) *Manager {
	return &Manager{
		st:         st,
		log:        log,
		bcryptGate: make(chan struct{}, maxBcryptConcurrent),
		failWin:    make(map[string][]time.Time),
	}
}

// ---- 登录防护：per-IP 滑动窗 ----

// ipBlocked 报告该 IP 是否已在窗口内累计 loginFailuresPerMin 次失败。
func (m *Manager) ipBlocked(ip string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	stamps := m.failWin[ip]
	live := stamps[:0]
	for _, t := range stamps {
		if now.Sub(t) < loginWindow {
			live = append(live, t)
		}
	}
	if len(live) != len(stamps) {
		m.failWin[ip] = live
	}
	return len(live) >= loginFailuresPerMin
}

// recordFailure 记一次失败；条目总量触顶 4096 时先整体清窗，仍满则淘汰
// 最近失败最早的 IP（线性扫描一次，规模有界）。
func (m *Manager) recordFailure(ip string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failWin[ip] = append(m.failWin[ip], now)
	if len(m.failWin) <= maxTrackedIPs {
		return
	}
	for k, stamps := range m.failWin {
		live := stamps[:0]
		for _, t := range stamps {
			if now.Sub(t) < loginWindow {
				live = append(live, t)
			}
		}
		if len(live) == 0 {
			delete(m.failWin, k)
		} else {
			m.failWin[k] = live
		}
	}
	if len(m.failWin) <= maxTrackedIPs {
		return
	}
	oldestKey := ""
	var oldest time.Time
	for k, stamps := range m.failWin {
		last := stamps[len(stamps)-1]
		if oldestKey == "" || last.Before(oldest) {
			oldestKey, oldest = k, last
		}
	}
	if oldestKey != "" {
		delete(m.failWin, oldestKey)
	}
}

// ---- bcrypt 闸 ----

// acquireBcrypt 进入全局 bcrypt 并发闸（≤2）。满载时排队（上限人数 32、等待
// 上限 5s），超限返回 ErrRateLimited。ctx 取消同样放行退出。
func (m *Manager) acquireBcrypt(ctx context.Context) error {
	select {
	case m.bcryptGate <- struct{}{}:
		return nil
	default:
	}
	if m.waiting.Load() >= maxBcryptWaiters {
		return ErrRateLimited
	}
	m.waiting.Add(1)
	defer m.waiting.Add(-1)
	timer := time.NewTimer(bcryptQueueWait)
	defer timer.Stop()
	select {
	case m.bcryptGate <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrRateLimited
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) releaseBcrypt() { <-m.bcryptGate }

// dummyBcrypt 对未知用户名执行一次同成本 bcrypt 校验（填充时序）。
// 哈希惰性生成一次——这是时序填充材料而非机密，固定内容无安全影响。
func (m *Manager) dummyBcrypt() {
	m.dummyOnce.Do(func() {
		m.dummyHash, m.dummyErr = bcrypt.GenerateFromPassword([]byte("meshconsole-timing-pad"), bcryptCost)
		if m.dummyErr != nil {
			m.log.Error("generate dummy bcrypt hash", "err", m.dummyErr.Error())
		}
	})
	if m.dummyErr != nil {
		return
	}
	_ = bcrypt.CompareHashAndPassword(m.dummyHash, []byte("meshconsole-timing-pad-input"))
}

// ---- 登录 / 会话 ----

// Login 校验用户名口令；成功签发 256bit 会话 token（明文返回一次，库中只存
// SHA-256）。失败统一 ErrInvalidCredentials/ErrRateLimited，不区分细节。
func (m *Manager) Login(ctx context.Context, ip, username, password string) (token string, err error) {
	now := time.Now()
	if m.ipBlocked(ip, now) {
		m.log.Warn("login blocked: per-ip failure window", "ip", ip)
		return "", ErrRateLimited
	}
	// bcrypt 闸在查询用户之前进入：无论用户是否存在都消耗一次闸位，
	// 保证未知用户名（dummy 路径）与有效用户名的排队行为一致。
	if err := m.acquireBcrypt(ctx); err != nil {
		m.log.Warn("login rejected: bcrypt gate saturated", "ip", ip)
		return "", err
	}
	defer m.releaseBcrypt()

	u, err := m.st.GetUserByName(ctx, username)
	if err != nil {
		m.log.Error("login get user", "err", err.Error())
		return "", ErrInvalidCredentials
	}
	if u == nil {
		m.dummyBcrypt()
		m.recordFailure(ip, now)
		return "", ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		m.recordFailure(ip, now)
		return "", ErrInvalidCredentials
	}
	if !u.Enabled {
		// 禁用用户的会话/令牌已在 disable 时全部撤销；此处兜底拒绝，
		// 文案仍与认证失败一致（不泄露「账号存在但被禁用」）。
		m.recordFailure(ip, now)
		return "", ErrInvalidCredentials
	}

	raw, err := newToken()
	if err != nil {
		m.log.Error("generate session token", "err", err.Error())
		return "", ErrInvalidCredentials
	}
	if m.afterValidate != nil {
		// 仅测试注入：此刻已读过旧哈希、bcrypt 校验已通过——钩子里发生的改密/
		// 禁用模拟「事务间隙先完成的撤销事务」。
		m.afterValidate()
	}
	created := time.Now()
	expires := created.Add(sessionTTL)
	if cap := created.Add(sessionAbsolute); expires.After(cap) {
		expires = cap
	}
	// 签发复核（R33-#1）：CreateSession 以单条条件插入校验「当前 password_version
	// 与启用态仍与凭据校验时一致」，不一致（凭据校验后改密/禁用事务已提交）即
	// ErrSessionIssuanceDenied——旧凭据签不出活会话，统一按认证失败返回。
	if err := m.st.CreateSession(ctx, hashToken(raw), u.ID, created.Unix(), expires.Unix(), u.PasswordVersion); err != nil {
		if errors.Is(err, store.ErrSessionIssuanceDenied) {
			m.log.Warn("login denied: user state changed between credential check and issuance", "user", u.Username)
			m.recordFailure(ip, time.Now())
			return "", ErrInvalidCredentials
		}
		m.log.Error("create session", "err", err.Error())
		return "", ErrInvalidCredentials
	}
	m.log.Info("user logged in", "user", u.Username, "ip", ip)
	return raw, nil
}

// Logout 撤销当前会话。
func (m *Manager) Logout(ctx context.Context, tokenHash string) error {
	return m.st.DeleteSession(ctx, tokenHash)
}

// Session 为一次通过校验的会话（含滑动续期结果）。
type Session struct {
	Username  string
	TokenHash string
}

// CheckSession 校验 Cookie 中的会话 token：存在、未过期（滑动+绝对双期限）、
// 用户仍启用；满足续期条件时滑动续期（节流 renewStep）。失败返回 ErrSessionExpired。
func (m *Manager) CheckSession(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrSessionExpired
	}
	h := hashToken(token)
	rec, err := m.st.GetSession(ctx, h)
	if err != nil {
		return nil, ErrSessionExpired
	}
	if rec == nil {
		return nil, ErrSessionExpired
	}
	now := time.Now()
	// 绝对期限：创建满 30 天一律失效（续期不越过）。
	if now.Unix() >= rec.CreatedAt+int64(sessionAbsolute/time.Second) {
		_ = m.st.DeleteSession(ctx, h)
		return nil, ErrSessionExpired
	}
	// 滑动期限：过期即失效。
	if now.Unix() >= rec.ExpiresAt {
		_ = m.st.DeleteSession(ctx, h)
		return nil, ErrSessionExpired
	}
	if !rec.Enabled {
		return nil, ErrSessionExpired
	}
	// 滑动续期：剩余寿命不足 sessionTTL-renewStep 时推满到 min(now+TTL, 绝对上限)，
	// 单会话至多每 renewStep 产生一次写事务。
	if remaining := time.Duration(rec.ExpiresAt-now.Unix()) * time.Second; remaining < sessionTTL-renewStep {
		expires := now.Add(sessionTTL)
		if cap := now.Add(sessionAbsolute - time.Duration(now.Unix()-rec.CreatedAt)*time.Second); expires.After(cap) {
			expires = cap
		}
		if err := m.st.TouchSession(ctx, h, expires.Unix()); err != nil {
			m.log.Error("touch session", "err", err.Error())
		}
	}
	return &Session{Username: rec.Username, TokenHash: h}, nil
}

// SessionCookie 构造会话 Cookie（SPEC §1：Path=/、host-only 不设 Domain、
// HttpOnly、Secure、SameSite=Lax）。
func SessionCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// ---- API token（MCP Bearer） ----

// CheckAPIToken 校验 MCP Bearer token：哈希查表 → constant-time 复核 → 用户
// 启用 → 未到期。任何失败返回 ErrUnauthorized（响应体中文化，不泄露工具列表）。
func (m *Manager) CheckAPIToken(ctx context.Context, bearer string) (*store.APITokenAuth, error) {
	if bearer == "" {
		return nil, ErrUnauthorized
	}
	rec, err := m.st.AuthenticateAPIToken(ctx, hashToken(bearer))
	if err != nil {
		m.log.Error("authenticate api token", "err", err.Error())
		return nil, ErrUnauthorized
	}
	if rec == nil {
		return nil, ErrUnauthorized
	}
	// DB 等值命中后再经应用层 constant-time 复核（与节点 token 同口径）：
	// 索引查找的耗时不保证常量，复核保证接受路径常量时间。
	if subtle.ConstantTimeCompare([]byte(rec.TokenHash), []byte(hashToken(bearer))) != 1 {
		return nil, ErrUnauthorized
	}
	if !rec.Enabled {
		return nil, ErrUnauthorized
	}
	if rec.ExpiresAt.Valid && time.Now().Unix() >= rec.ExpiresAt.Int64 {
		return nil, ErrUnauthorized
	}
	return rec, nil
}

// SessionCleanupLoop 周期清理过期会话行（服务模式后台循环）：登录/登出增删
// sessions 行，过期行若不回收会无限累积（含被撤销用户的残留）。间隔由调用方
// 传入（默认 10 分钟，远大于任何会话语义窗口）。
func SessionCleanupLoop(ctx context.Context, st *store.Store, log logger, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	log.Info("session cleanup loop started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := st.DeleteExpiredSessions(ctx, time.Now().Unix())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("cleanup expired sessions", "err", err.Error())
			continue
		}
		if n > 0 {
			log.Info("expired sessions cleaned", "count", n)
		}
	}
}

// ---- 工具 ----

// newToken 生成 32 字节（256bit）随机 hex token。
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken 为 token 明文的 SHA-256 hex（会话与 API token 同口径）。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashToken 为 hashToken 的导出形态（CLI `token create` 落库同一口径，
// 避免哈希逻辑在两处漂移）。
func HashToken(token string) string { return hashToken(token) }

// HashPassword 生成 bcrypt 哈希（cost 10，SPEC §1）；口令长度下限在此统一把关。
func HashPassword(password string) (string, error) {
	if len(password) < minPasswordLen {
		return "", errors.New("口令长度不得少于 8 字符")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// NewAPIToken 生成带前缀的 API token 明文（mcp_ + 256bit hex）。
func NewAPIToken() (string, error) {
	raw, err := newToken()
	if err != nil {
		return "", err
	}
	return APITokenPrefix + raw, nil
}

// BearerToken 提取 Authorization: Bearer 头的 token 部分。
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
