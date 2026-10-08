package panel

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/meshview"
	"github.com/mtl802/meshconsole/internal/store"
)

func testLogger() *slog.Logger {
	// 面板访问日志为 DEBUG 级：测试里丢弃即可。
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

// newTestPanel 建库（一在线节点带服务/agent/tailnet）+ 注册好面板路由的 mux。
func newTestPanel(t *testing.T) *http.ServeMux {
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

	mux := http.NewServeMux()
	New(meshview.New(st), testLogger()).RegisterRoutes(mux)
	return mux
}

// do 以指定 Host/Origin 发请求（httptest.NewRequest 允许覆写 Host 头）。
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

// TestGuardHostAllowlist Host 校验（SPEC-M1b-b §4.2 + R19-#7 严格化）：仅
// localhost/127.0.0.1/[::1]（带或不带端口、大小写不敏感）放行；DNS rebinding
// 变体与一切畸形形式（非数字/越界/空端口、方括号非 IPv6 字面量、裸 IPv6、
// userinfo、携带 path、空白）一律 403。
func TestGuardHostAllowlist(t *testing.T) {
	mux := newTestPanel(t)
	allowed := []string{
		"localhost", "localhost:7700", "127.0.0.1", "127.0.0.1:7700",
		"[::1]", "[::1]:7700", "LOCALHOST:7700", "LocalHost",
	}
	for _, h := range allowed {
		if rec := do(mux, "GET", "/api/panel/overview", h, ""); rec.Code != http.StatusOK {
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
		if rec := do(mux, "GET", "/api/panel/overview", h, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("host %q: code = %d, want 403", h, rec.Code)
		}
	}
}

// TestGuardOrigin Origin 头存在时校验同一名单；null/Opaque 拒绝；跨源拒绝；
// R19-#7：scheme 限 http(s)，带 userinfo/path 的畸形形式同样拒绝。
func TestGuardOrigin(t *testing.T) {
	mux := newTestPanel(t)
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
		if rec := do(mux, "GET", "/api/panel/overview", "localhost:7700", o); rec.Code != http.StatusForbidden {
			t.Fatalf("origin %q: code = %d, want 403", o, rec.Code)
		}
	}
	for _, o := range []string{"http://localhost:7700", "https://localhost", "http://127.0.0.1:7700", "http://[::1]:7700"} {
		if rec := do(mux, "GET", "/api/panel/overview", "localhost:7700", o); rec.Code != http.StatusOK {
			t.Fatalf("origin %q: code = %d, want 200", o, rec.Code)
		}
	}
}

// TestGuardSecurityHeaders 安全响应头（nosniff/no-store）在成功与失败路径都要有。
func TestGuardSecurityHeaders(t *testing.T) {
	mux := newTestPanel(t)
	for _, host := range []string{"localhost:7700", "evil.com"} {
		rec := do(mux, "GET", "/", host, "")
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
	mux := newTestPanel(t)
	rec := do(mux, "GET", "/api/panel/overview", "127.0.0.1:7700", "")
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
// `curl / | grep glass` 有料），CSS/JS 可取且类型正确，未知路径 404。
func TestStaticServed(t *testing.T) {
	mux := newTestPanel(t)
	rec := do(mux, "GET", "/", "localhost:7700", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"glass-surface", "glass-surface-soft", "glass.css", "app.js", "READ-ONLY"} {
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
	if rec = do(mux, "POST", "/api/panel/overview", "localhost:7700", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST overview: code = %d, want 405", rec.Code)
	}
}
