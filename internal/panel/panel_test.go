package panel

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/auth"
	"github.com/mtl802/meshconsole/internal/meshview"
	"github.com/mtl802/meshconsole/internal/store"
)

func testLogger() *slog.Logger {
	// 面板访问日志为 DEBUG 级：测试里丢弃即可。
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testUser / testPass 为面板测试账号（bcrypt cost 10，登录路径真实走一遍）。
const (
	testUser = "admin"
	testPass = "panel-test-password"
)

// newTestPanel 建库（一在线节点带服务/agent/tailnet）+ 注册好面板路由的 mux，
// 并预置测试账号；返回 mux 与「已登录会话」的 Cookie（后续请求带它会话守卫放行）。
func newTestPanel(t *testing.T) (*http.ServeMux, *http.Cookie) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	n, err := st.RegisterNode(ctx, "cloud-1", "server", "linux", "amd64", "h1", "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	cpu := 21.0
	memU, memT := int64(1<<30), int64(4<<30)
	_, err = st.HeartbeatFull(ctx, &store.MetricsRow{
		NodeID: n.ID, TS: time.Now().Unix(),
		CPUPct: &cpu, MemUsed: &memU, MemTotal: &memT,
	}, "agent-1",
		&[]store.ServiceRow{{Name: "headscale", Type: "systemd", Status: "active"}},
		&[]store.AgentRow{{Name: "zcode", Type: "cli", Version: "3.14.4", Status: "active"}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceTailnetNodes(ctx, []store.TailnetNodeRow{
		{ID: 1, MachineName: "cloud-1", IPs: "100.64.0.1", Online: true},
	}); err != nil {
		t.Fatal(err)
	}

	// 预置账号（store 直建，bcrypt 口径与 CLI 相同）。
	hash, err := auth.HashPassword(testPass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, testUser, hash); err != nil {
		t.Fatal(err)
	}

	authMgr := auth.New(st, testLogger())
	mux := http.NewServeMux()
	New(meshview.New(st), testLogger(), authMgr, nil).RegisterRoutes(mux)

	// 走真实登录流程取得会话 Cookie（POST /login → Set-Cookie mc_session）。
	form := url.Values{"username": {testUser}, "password": {testPass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "localhost:7700"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login: code = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Value == "" {
		t.Fatal("login: no session cookie set")
	}
	return mux, cookie[0]
}

// do 以指定 Host/Origin 发匿名请求（httptest.NewRequest 允许覆写 Host 头）。
func do(mux *http.ServeMux, method, target, host, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// doAuth 同 do，携带会话 Cookie（会话守卫放行形态）。
func doAuth(mux *http.ServeMux, cookie *http.Cookie, method, target, host, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestSessionGuard 会话守卫（SPEC-M1b-c2 §1）：未登录一律 302 /login（含
// /api/panel/*——不得泄露业务数据）；豁免清单（GET /login、静态资源）匿名
// 可及；带会话 Cookie 正常放行；无效会话 token 同样 302。
func TestSessionGuard(t *testing.T) {
	mux, cookie := newTestPanel(t)
	// 未登录：面板与 API 全 302（SPEC §1：含 /api/panel/*——不得泄露业务数据；
	// 未匹配任何路由的路径由 mux 直接 404，无资源可泄露，不属「面板资源」）。
	for _, path := range []string{"/", "/api/panel/overview"} {
		rec := do(mux, "GET", path, "localhost:7700", "")
		if rec.Code != http.StatusFound {
			t.Fatalf("anonymous GET %s: code = %d, want 302", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Fatalf("anonymous GET %s: Location = %q, want /login", path, loc)
		}
		if strings.Contains(rec.Body.String(), "cloud-1") {
			t.Fatalf("anonymous GET %s leaked business data", path)
		}
	}
	// 豁免清单匿名可及。
	for _, path := range []string{"/login", "/glass.css", "/app.js"} {
		rec := do(mux, "GET", path, "localhost:7700", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("exempt GET %s: code = %d, want 200", path, rec.Code)
		}
	}
	// 登录后放行。
	if rec := doAuth(mux, cookie, "GET", "/api/panel/overview", "localhost:7700", ""); rec.Code != http.StatusOK {
		t.Fatalf("authed overview: code = %d, want 200", rec.Code)
	}
	if rec := doAuth(mux, cookie, "GET", "/", "localhost:7700", ""); rec.Code != http.StatusOK {
		t.Fatalf("authed index: code = %d, want 200", rec.Code)
	}
	// 无效会话 token 同样 302。
	bad := *cookie
	bad.Value = "deadbeef"
	if rec := doAuth(mux, &bad, "GET", "/", "localhost:7700", ""); rec.Code != http.StatusFound {
		t.Fatalf("bad session: code = %d, want 302", rec.Code)
	}
}

// TestLoginFlow 登录页与登录动作：错密码 401 + 统一文案；同一 IP 连续 5 次失败
// 后第 6 次 429（SPEC §1 per-IP 5 次/分）；限流窗口内正确密码同样 429
// （限先于验证）。
func TestLoginFlow(t *testing.T) {
	mux, _ := newTestPanel(t)
	// 登录页可匿名取得且含中文表单。
	rec := do(mux, "GET", "/login", "localhost:7700", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "登录控制台") {
		t.Fatalf("login page: code = %d", rec.Code)
	}
	post := func(user, pass string) *httptest.ResponseRecorder {
		form := url.Values{"username": {user}, "password": {pass}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "localhost:7700"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	// 错密码/未知用户：401 + 统一文案（不泄露用户是否存在）。
	rec = post(testUser, "wrong-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: code = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), auth.ErrInvalidCredentials.Error()) {
		t.Fatal("wrong password: unified message missing")
	}
	rec = post("no-such-user", "wrong-password")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "用户名或密码错误") {
		t.Fatalf("unknown user: code = %d", rec.Code)
	}
	// 已失败 4 次（上两发 + 下方两发），第 5 次仍 401，第 6 次 429。
	for i := 0; i < 2; i++ {
		if rec := post(testUser, "wrong-password"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code = %d, want 401", i, rec.Code)
		}
	}
	if rec := post(testUser, "wrong-password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("5th attempt: code = %d, want 401", rec.Code)
	}
	if rec := post(testUser, "wrong-password"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: code = %d, want 429 (per-ip 5/min)", rec.Code)
	}
	// 正确密码在限流窗口内同样被 429（限先于验证）。
	if rec := post(testUser, testPass); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("valid password under limit: code = %d, want 429", rec.Code)
	}
}

// TestCookieAttributes 会话 Cookie 属性（SPEC §1）：HttpOnly、Secure、
// SameSite=Lax、Path=/、host-only（不设 Domain）、256bit token。
func TestCookieAttributes(t *testing.T) {
	mux, _ := newTestPanel(t)
	form := url.Values{"username": {testUser}, "password": {testPass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "localhost:7700"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != "mc_session" {
		t.Fatalf("cookie name = %q", c.Name)
	}
	if !c.HttpOnly || !c.Secure {
		t.Fatalf("HttpOnly=%v Secure=%v", c.HttpOnly, c.Secure)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v", c.SameSite)
	}
	if c.Path != "/" || c.Domain != "" {
		t.Fatalf("Path=%q Domain=%q", c.Path, c.Domain)
	}
	if len(c.Value) != 64 { // 256bit hex
		t.Fatalf("token len = %d, want 64", len(c.Value))
	}
}

// TestLogout 登出（SPEC §8 验收「登出失效」）：登出后会话立即失效——同一
// Cookie 再访问 / 一律 302 /login。
func TestLogout(t *testing.T) {
	mux, cookie := newTestPanel(t)
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Host = "localhost:7700"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("logout: code = %d", rec.Code)
	}
	// 原 Cookie 已失效。
	if rec := doAuth(mux, cookie, "GET", "/", "localhost:7700", ""); rec.Code != http.StatusFound {
		t.Fatalf("post-logout session must be invalid: code = %d, want 302", rec.Code)
	}
}

// TestGuardHostAllowlist Host 校验（SPEC-M1b-c2 §4 配置化 + R19-#7 严格化）：
// 缺省名单（localhost/127.0.0.1/[::1]，带或不带端口、大小写不敏感）放行；
// DNS rebinding 变体与一切畸形形式（非数字/越界/空端口、方括号非 IPv6 字面量、
// 裸 IPv6、userinfo、携带 path、空白）一律 403。
func TestGuardHostAllowlist(t *testing.T) {
	mux, cookie := newTestPanel(t)
	allowed := []string{
		"localhost", "localhost:7700", "127.0.0.1", "127.0.0.1:7700",
		"[::1]", "[::1]:7700", "LOCALHOST:7700", "LocalHost",
	}
	for _, h := range allowed {
		if rec := doAuth(mux, cookie, "GET", "/api/panel/overview", h, ""); rec.Code != http.StatusOK {
			t.Fatalf("host %q: code = %d, want 200 (body: %s)", h, rec.Code, rec.Body.String())
		}
	}
	rejected := []string{
		"evil.com", "evil.com:80",
		"127.0.0.1.evil.com",     // 后缀伪装
		"attacker.com:127.0.0.1", // SPEC 点名的 rebinding 变体（非数字端口）
		"localhost.evil.com",
		"", // HTTP/1.0 无 Host
		// R19-#7 畸形形式变体：
		"localhost:evil",  // 非数字端口
		"localhost:",      // 空端口
		"[::1]:",          // 方括号 + 空端口
		"127.0.0.1:0",     // 端口 0
		"127.0.0.1:99999", // 端口越界
		"[localhost]",     // 方括号非 IPv6 字面量
		"[127.0.0.1]",     // 方括号非 IPv6 字面量
		"::1",             // 裸 IPv6（合法形式须方括号）
		"evil@localhost",  // userinfo
		"localhost/evil",  // 携带 path
		" localhost",      // 首部空白
	}
	for _, h := range rejected {
		if rec := doAuth(mux, cookie, "GET", "/api/panel/overview", h, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("host %q: code = %d, want 403", h, rec.Code)
		}
	}
}

// TestGuardOrigin Origin 头存在时校验同一名单；null/Opaque 拒绝；跨源拒绝；
// R19-#7：scheme 限 http(s)，带 userinfo/path 的畸形形式同样拒绝。
func TestGuardOrigin(t *testing.T) {
	mux, cookie := newTestPanel(t)
	rejected := []string{
		"http://evil.com",
		"null",
		"ftp://localhost",            // scheme 白名单外
		"chrome-extension://abc",     // scheme 白名单外
		"http://evil@localhost:7700", // userinfo
		"http://localhost:7700/evil", // 携带 path
		"http://localhost:7700/?x=1", // 携带 query
		"localhost:7700",             // 无 scheme
	}
	for _, o := range rejected {
		if rec := doAuth(mux, cookie, "GET", "/api/panel/overview", "localhost:7700", o); rec.Code != http.StatusForbidden {
			t.Fatalf("origin %q: code = %d, want 403", o, rec.Code)
		}
	}
	for _, o := range []string{"http://localhost:7700", "https://localhost", "http://127.0.0.1:7700", "http://[::1]:7700"} {
		if rec := doAuth(mux, cookie, "GET", "/api/panel/overview", "localhost:7700", o); rec.Code != http.StatusOK {
			t.Fatalf("origin %q: code = %d, want 200", o, rec.Code)
		}
	}
}

// TestGuardSecurityHeaders 安全响应头（nosniff/no-store）在成功与失败路径都要有。
func TestGuardSecurityHeaders(t *testing.T) {
	mux, cookie := newTestPanel(t)
	for _, host := range []string{"localhost:7700", "evil.com"} {
		rec := doAuth(mux, cookie, "GET", "/", host, "")
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("host %q: X-Content-Type-Options = %q", host, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("host %q: Cache-Control = %q", host, got)
		}
	}
}

// TestOverviewEndpoint 聚合 JSON（SPEC-M1b-b §4.1）：nodes/services/agents/
// tailnet/metrics 摘要一次取齐。
func TestOverviewEndpoint(t *testing.T) {
	mux, cookie := newTestPanel(t)
	rec := doAuth(mux, cookie, "GET", "/api/panel/overview", "127.0.0.1:7700", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var ov meshview.Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(ov.Nodes) != 1 || ov.Nodes[0].Node.Name != "cloud-1" {
		t.Fatalf("nodes = %+v", ov.Nodes)
	}
	if ov.Nodes[0].Latest == nil || ov.Nodes[0].Latest.CPUPct == nil {
		t.Fatalf("latest metrics missing: %+v", ov.Nodes[0])
	}
	if len(ov.Services) != 1 || len(ov.Agents) != 1 {
		t.Fatalf("services=%d agents=%d", len(ov.Services), len(ov.Agents))
	}
	if ov.Tailnet == nil || ov.Tailnet.Tracked != 1 {
		t.Fatalf("tailnet = %+v", ov.Tailnet)
	}
	if ov.Freshness == nil {
		t.Fatal("freshness missing")
	}
}

// TestStaticServed 静态页（embed）：根路径 HTML 含 glass 材质类名（SPEC §4.4
// `curl / | grep glass` 有料；首页中文化为 M1b-c2 §3），CSS/JS 可取且类型正确，
// 未知路径 404。
func TestStaticServed(t *testing.T) {
	mux, cookie := newTestPanel(t)
	rec := doAuth(mux, cookie, "GET", "/", "localhost:7700", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"glass-surface", "glass-surface-soft", "glass.css", "app.js", "只读"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	// glass-inset 语义由 JS 行构建承载（SPEC §4.4 `curl / | grep glass` 在 HTML
	// 源码已可命中多处 glass-surface；inset 类名在 app.js 里核对）。
	js := do(mux, "GET", "/app.js", "localhost:7700", "").Body.String()
	if !strings.Contains(js, "glass-inset") {
		t.Fatal("app.js rows must use glass-inset semantics")
	}
	rec = do(mux, "GET", "/glass.css", "localhost:7700", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("glass.css: code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "backdrop-filter") {
		t.Fatal("glass.css must carry real frost (backdrop-filter)")
	}
	rec = do(mux, "GET", "/app.js", "localhost:7700", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js: code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec = do(mux, "GET", "/nope.js", "localhost:7700", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: code = %d, want 404", rec.Code)
	}
	// 面板路由不越权：POST 到面板路径不匹配（方法限定 GET）。
	if rec = doAuth(mux, cookie, "POST", "/api/panel/overview", "localhost:7700", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST overview: code = %d, want 405", rec.Code)
	}
}
