// registry_m1bc2_test.go 为 agent API 来源收敛测试（SPEC-M1b-c2 §5）：
// 缺省网段（回环/私网/Tailnet CGNAT）放行、公网来源 403、IPv4-mapped 规范化、
// agent_allowed_cidrs 覆写（缺省 nil=缺省清单；显式空=仅回环）、token 校验
// 独立保留（来源放行 ≠ 身份认证）、错误体中文化。
package registry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mtl802/meshconsole/internal/store"
)

func testLogger2() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
}

// cidrs 解析测试网段（非法值直接 panic——测试输入可控）。
func cidrs(ss ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("bad test cidr " + s)
		}
		out = append(out, n)
	}
	return out
}

// TestSourceAllowedUnit sourceAllowed 纯函数：缺省网段语义 + IPv4-mapped
// 规范化（::ffff:100.64.0.3 按IPv4 比对）。
func TestSourceAllowedUnit(t *testing.T) {
	def := defaultSourceCIDRs()
	allowed := []string{
		"127.0.0.1:40000", "127.8.9.9:1", // 回环 127/8
		"[::1]:40000",                                 // IPv6 回环
		"[::ffff:127.0.0.1]:10",                       // v4-mapped 回环
		"10.1.2.3:5", "172.16.0.9:6", "192.168.1.1:7", // 私网
		"100.64.0.3:8", "100.127.255.254:8", // Tailnet CGNAT 段
		"[::ffff:100.64.0.3]:8", // v4-mapped tailnet
	}
	for _, a := range allowed {
		if !sourceAllowed(a, def) {
			t.Fatalf("addr %q must be allowed", a)
		}
	}
	rejected := []string{
		"1.13.158.180:7700", // 公网
		"8.8.8.8:53",
		"100.128.0.1:8", // CGNAT 段末之外（100.64/10 到 100.127.255.255）
		"11.0.0.1:5",
		"",
		"not-an-addr",
		"1.2.3.4", // 无端口（SplitHostPort 失败后整串非 IP）
	}
	for _, a := range rejected {
		if sourceAllowed(a, def) {
			t.Fatalf("addr %q must be rejected", a)
		}
	}
}

// TestSourceFilterMiddleware HTTP 层：公网来源 403 + 中文化错误体，且先于
// token 认证（无 token 也 403 不 401）；回环来源 + 缺 token 仍 401（网络过滤
// 非身份认证，token 校验独立保留）；不读转发头。
func TestSourceFilterMiddleware(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := New(st, testLogger2(), nil, nil) // nil → 缺省网段
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	do := func(remote, fwd string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/agent/register", bytes.NewReader([]byte(`{"name":"n1"}`)))
		req.RemoteAddr = remote
		if fwd != "" {
			req.Header.Set("X-Forwarded-For", fwd)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	// 公网来源：403（即使伪造内网转发头也不放行——不读任何转发头）。
	rec := do("1.13.158.180:5555", "10.0.0.1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public source = %d, want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "来源地址不在允许网段" {
		t.Fatalf("error body = %q", body["error"])
	}
	// 回环来源但无 token：401（认证独立保留）。
	rec = do("127.0.0.1:5555", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("loopback no token = %d, want 401", rec.Code)
	}
}

// TestSourceCIDROverrides agent_allowed_cidrs 语义（SPEC §5）：显式空数组 =
// 仅回环（tailnet 来源被拒）；自定义网段覆写生效。
func TestSourceCIDROverrides(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "src2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// 显式空 = 仅回环。
	h := New(st, testLogger2(), nil, cidrs("127.0.0.0/8", "::1/128"))
	if sourceAllowed("100.64.0.3:8", h.srcCIDRs) {
		t.Fatal("explicit-empty must reject tailnet source")
	}
	if !sourceAllowed("127.0.0.1:8", h.srcCIDRs) {
		t.Fatal("explicit-empty must accept loopback")
	}
	// 自定义网段：仅 100.64/10。
	h2 := New(st, testLogger2(), nil, cidrs("100.64.0.0/10"))
	if !sourceAllowed("100.64.0.9:8", h2.srcCIDRs) {
		t.Fatal("custom cidr must accept 100.64/10")
	}
	if sourceAllowed("127.0.0.1:8", h2.srcCIDRs) {
		t.Fatal("custom cidr replaces defaults (loopback not implied)")
	}
}
