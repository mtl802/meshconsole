// Package panel 为 Web 只读面板（SPEC-M1b-b §4）：静态资源 embed 进二进制 +
// /api/panel/overview 聚合 JSON 端点（数据经 internal/meshview，与 MCP 同源）。
//
// 安全口径（DESIGN §8）：
//   - 面板与 agent API 同端口，仅本机/隧道可达；
//   - Host/Origin 校验：非 localhost 名单的 Host 一律 403（防 DNS rebinding，
//     如 attacker.com 解析到 127.0.0.1 的变体同样因 Host 名单外被拒）；
//   - 响应头 X-Content-Type-Options: nosniff、Cache-Control: no-store；
//   - 无 Cookie/无状态只读，无 CSRF 面；
//   - 访问日志打 DEBUG（不打 INFO 刷屏）。
package panel

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/meshview"
)

//go:embed all:web
var embedded embed.FS

// contentTypes 为静态文件的手工映射（不开放目录列表/未知路径 404，面越小越好）。
var contentTypes = map[string]string{
	"index.html": "text/html; charset=utf-8",
	"glass.css":  "text/css; charset=utf-8",
	"app.js":     "text/javascript; charset=utf-8",
}

// Panel 为面板处理器集合。
type Panel struct {
	q   *meshview.Query
	log *slog.Logger
	web fs.FS
}

func New(q *meshview.Query, log *slog.Logger) *Panel {
	sub, err := fs.Sub(embedded, "web")
	if err != nil {
		// embed 布局编译期确定，不可能失败；防御性兜底。
		panic("panel: embed web/: " + err.Error())
	}
	return &Panel{q: q, log: log, web: sub}
}

// RegisterRoutes 把面板路由挂到 mux（GET / 与 GET /api/panel/overview）。
// /api/agent/* 由 registry 独立注册，不在本中间件覆盖范围（其自带 token 认证）。
func (p *Panel) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /{$}", p.guard(p.static("index.html")))
	mux.Handle("GET /glass.css", p.guard(p.static("glass.css")))
	mux.Handle("GET /app.js", p.guard(p.static("app.js")))
	mux.Handle("GET /api/panel/overview", p.guard(http.HandlerFunc(p.handleOverview)))
}

// hostAllowed 报告 Host/Origin 的主机名是否在允许名单（localhost/127.0.0.1/
// [::1]，带或不带端口）。R19-#7 严格化：url.Parse 解析 authority 后对
// hostname 严格比对白名单——非数字/越界/空端口、方括号非 IPv6 字面量
// （[localhost]）、userinfo（evil@localhost）、携带 path/query、首尾空白等
// 畸形形式一律拒绝；Host 为空（HTTP/1.0 无 Host 头）同样拒绝。裸 IPv6
// （未加方括号的 "::1"）解析不出合法 authority，按畸形拒绝（方括号形式
// [::1] 才是 RFC 3986 合法写法）。
func hostAllowed(hostport string) bool {
	raw := hostport
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	u, err := url.Parse("http://" + raw)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	// 端口严格值域：url.Parse 只查数字形态，这里补空端口与 1-65535 校验。
	port := u.Port()
	if port == "" && strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return false
		}
	}
	host := strings.ToLower(u.Hostname())
	// 方括号是 IP-literal 的保留形式：仅接受 IPv6 字面量（[::1]），
	// [localhost] / [127.0.0.1] 等畸形一律拒绝。
	if strings.Contains(raw, "[") {
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() != nil {
			return false
		}
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// originAllowed 校验 Origin 头（存在时）：必须是 scheme 限 http/https 的
// 绝对 URL 且主机名在名单内；null/Opaque Origin、无 scheme、其他 scheme
// （ftp/chrome-extension 等）、带 userinfo 或 path/query/fragment 的畸形
// 形式一律拒绝（R19-#7）。
func originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return hostAllowed(u.Host)
}

// guard 为面板路由公共壳：安全响应头 + Host/Origin 校验 + DEBUG 访问日志。
// 头在任何路径（含 403）都先于 body 设置——nosniff/no-store 是面板响应的
// 固定属性，不因拒绝而豁免。
func (p *Panel) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// 无 Cookie/无状态只读：无 CSRF 面；nosniff + no-store 固定携带。
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if !hostAllowed(r.Host) {
			p.log.Debug("panel request blocked: host not allowed", "host", r.Host, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !originAllowed(o) {
			p.log.Debug("panel request blocked: origin not allowed", "origin", o, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		p.log.Debug("panel request", "method", r.Method, "path", r.URL.Path, "host", r.Host)
		next.ServeHTTP(w, r)
		p.log.Debug("panel request done", "path", r.URL.Path, "elapsed_ms", time.Since(start).Milliseconds())
	})
}

// static 返回指定静态文件的 handler（内容 embed 进二进制，离线可用）。
func (p *Panel) static(name string) http.Handler {
	data, err := fs.ReadFile(p.web, name)
	if err != nil {
		panic("panel: embedded file missing: " + name)
	}
	ct := contentTypes[name]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write(data)
	})
}

// handleOverview 输出聚合 JSON（nodes/services/agents/tailnet/metrics 摘要）。
func (p *Panel) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := p.q.Overview(r.Context())
	if err != nil {
		p.log.Error("panel overview", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(ov); err != nil {
		p.log.Error("panel overview encode", "err", err)
	}
}
