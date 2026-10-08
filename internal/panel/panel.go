// Package panel 为 Web 只读面板（SPEC-M1b-b §4 + SPEC-M1b-c2 §1/§4/§7）：
// 静态资源 embed 进二进制 + /api/panel/overview 聚合 JSON 端点（数据经
// internal/meshview，与 MCP 同源）+ 账号会话认证。
//
// 安全口径（DESIGN §8、SPEC-M1b-c2 §1）：
//   - 认证在所有网络形态始终开启（含回环 listen）——面板全部资源挂会话守卫，
//     未登录一律 302 /login；豁免清单显式：GET /login（登录页）、POST /login
//     （登录动作，受登录防护约束）、静态资源（css/js）、GET /healthz（调用方
//     注册）。匿名可及仅此几项，其余（含 /api/panel/*）不得泄露业务数据与
//     工具元数据；
//   - Host/Origin 校验：白名单由 config panel_allowed_hosts 驱动（缺省回环
//     名单），解析严格性保留（R19-#7：畸形 authority 一律拒绝，防 DNS
//     rebinding）；
//   - 响应头 X-Content-Type-Options: nosniff、Cache-Control: no-store 全链
//     携带（含 403/登录页）；
//   - Cookie 仅 HttpOnly+Secure+SameSite=Lax（internal/auth SessionCookie）；
//     CSRF 面 = 登录/登出两个 POST，SameSite=Lax + Origin 校验覆盖；
//   - overview 挂独立并发闸 8（SPEC §7：与 MCP 的 4 并行，防挤占）；
//   - 访问日志打 DEBUG（不打 INFO 刷屏）。
package panel

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/auth"
	"github.com/mtl802/meshconsole/internal/cmdkind"
	"github.com/mtl802/meshconsole/internal/cmdsvc"
	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/meshview"
	"github.com/mtl802/meshconsole/internal/store"
)

//go:embed all:web
var embedded embed.FS

// contentTypes 为静态文件的手工映射（不开放目录列表/未知路径 404，面越小越好）。
var contentTypes = map[string]string{
	"index.html": "text/html; charset=utf-8",
	"login.html": "text/html; charset=utf-8",
	"glass.css":  "text/css; charset=utf-8",
	"app.js":     "text/javascript; charset=utf-8",
}

// overviewConcurrent 为 overview 端点并发上限（SPEC-M1b-c2 §7：semaphore 8，
// 与 MCP 的 4 独立并行）。
const overviewConcurrent = 8

// maxLoginBody 为登录表单体上限（用户名/口令字段远小于该值，超限即拒绝）。
const maxLoginBody = 4 << 10

// maxCommandFormBody 为命令提交表单体上限（几个短字段，超限即拒绝）。
const maxCommandFormBody = 8 << 10

// loginPageData 为登录页模板数据（Error 文案来自包内常量，无用户可控内容，
// 仍经 html/template 转义兜底）。
type loginPageData struct {
	Error string
}

// Panel 为面板处理器集合。
type Panel struct {
	q     *meshview.Query
	st    *store.Store
	cfg   *config.Console
	log   *slog.Logger
	web   fs.FS
	auth  *auth.Manager
	hosts []string // 规范化后的 Host 白名单（config 加载期校验）
}

// New 构造面板处理器。allowedHosts 为 Host/Origin 白名单（裸主机名/IP，调用方
// 须传 config.AllowedHosts() 规范化产物；nil 时回退缺省回环名单，便于测试）。
// st + cfg 承载 M1d 命令提交（面板单管理员 = operator，SPEC-M1d §1/§4）。
func New(q *meshview.Query, st *store.Store, cfg *config.Console, log *slog.Logger, am *auth.Manager, allowedHosts []string) *Panel {
	sub, err := fs.Sub(embedded, "web")
	if err != nil {
		// embed 布局编译期确定，不可能失败；防御性兜底。
		panic("panel: embed web/: " + err.Error())
	}
	if allowedHosts == nil {
		allowedHosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	return &Panel{q: q, st: st, cfg: cfg, log: log, web: sub, auth: am, hosts: allowedHosts}
}

// RegisterRoutes 把面板路由挂到 mux。
// 豁免清单（匿名可及，SPEC-M1b-c2 §1）：GET /login、POST /login、静态资源
// （/glass.css、/app.js）；GET /healthz 由调用方单独注册。其余全部会话守卫。
// /api/agent/* 由 registry 独立注册，不在本中间件覆盖范围（自带 token 认证与
// 来源收敛）。
func (p *Panel) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /{$}", p.guard(p.requireSession(p.static("index.html"))))
	mux.Handle("GET /glass.css", p.guard(p.static("glass.css")))
	mux.Handle("GET /app.js", p.guard(p.static("app.js")))
	mux.Handle("GET /login", p.guard(http.HandlerFunc(p.handleLoginPage)))
	mux.Handle("POST /login", p.guard(http.HandlerFunc(p.handleLogin)))
	mux.Handle("POST /logout", p.guard(http.HandlerFunc(p.handleLogout)))
	sem := make(chan struct{}, overviewConcurrent)
	mux.Handle("GET /api/panel/overview",
		p.guard(p.requireSession(limitConcurrent(sem, http.HandlerFunc(p.handleOverview)))))
	// M1d 命令通道（SPEC-M1d §4）：查询/元数据（GET）与提交（POST，面板单管理员
	// = operator）。同一会话守卫 + Host/Origin 校验（guard）覆盖；POST 同源表单
	// 由会话 Cookie SameSite=Lax 覆盖 CSRF 面（SPEC §4）。
	semCmd := make(chan struct{}, overviewConcurrent)
	mux.Handle("GET /api/panel/command_meta",
		p.guard(p.requireSession(limitConcurrent(semCmd, http.HandlerFunc(p.handleCommandMeta)))))
	mux.Handle("GET /api/panel/commands",
		p.guard(p.requireSession(limitConcurrent(semCmd, http.HandlerFunc(p.handleCommandList)))))
	mux.Handle("POST /api/panel/commands",
		p.guard(p.requireSession(limitConcurrent(semCmd, http.HandlerFunc(p.handleCommandSubmit)))))
}

// limitConcurrent 用信号量限制在处理请求数，超限 503（SPEC §7 overview 配额 8）。
func limitConcurrent(sem chan struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "繁忙，请稍后再试", http.StatusServiceUnavailable)
		}
	})
}

// hostAllowed 报告 Host/Origin 的主机名是否在允许名单（带或不带端口）。
// R19-#7 严格化保留：url.Parse 解析 authority 后对 hostname 严格比对白名单——
// 非数字/越界/空端口、方括号非 IPv6 字面量（[localhost]）、userinfo
// （evil@localhost）、携带 path/query、首尾空白等畸形形式一律拒绝；Host 为空
// （HTTP/1.0 无 Host 头）同样拒绝。裸 IPv6（未加方括号的 "::1"）解析不出合法
// authority，按畸形拒绝（方括号形式 [::1] 才是 RFC 3986 合法写法）。
// 名单由 config panel_allowed_hosts 驱动（SPEC-M1b-c2 §4），缺省回环名单。
func (p *Panel) hostAllowed(hostport string) bool {
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
	for _, h := range p.hosts {
		if host == h {
			return true
		}
	}
	return false
}

// originAllowed 校验 Origin 头（存在时）：必须是 scheme 限 http/https 的
// 绝对 URL 且主机名在名单内；null/Opaque Origin、无 scheme、其他 scheme
// （ftp/chrome-extension 等）、带 userinfo 或 path/query/fragment 的畸形
// 形式一律拒绝（R19-#7）。
func (p *Panel) originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return p.hostAllowed(u.Host)
}

// guard 为面板路由公共壳：安全响应头 + Host/Origin 校验 + DEBUG 访问日志。
// 头在任何路径（含 403）都先于 body 设置——nosniff/no-store 是面板响应的
// 固定属性，不因拒绝而豁免。
func (p *Panel) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if !p.hostAllowed(r.Host) {
			p.log.Debug("panel request blocked: host not allowed", "host", r.Host, "path", r.URL.Path)
			http.Error(w, "禁止访问", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !p.originAllowed(o) {
			p.log.Debug("panel request blocked: origin not allowed", "origin", o, "path", r.URL.Path)
			http.Error(w, "禁止访问", http.StatusForbidden)
			return
		}
		p.log.Debug("panel request", "method", r.Method, "path", r.URL.Path, "host", r.Host)
		next.ServeHTTP(w, r)
		p.log.Debug("panel request done", "path", r.URL.Path, "elapsed_ms", time.Since(start).Milliseconds())
	})
}

// sessionToken 从请求 Cookie 取会话 token。
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// requireSession 为会话守卫（SPEC-M1b-c2 §1）：未登录/会话失效一律 302 /login
// （含 /api/panel/*——API 响应同样是重定向，不落任何业务数据）。会话在本面板
// 只作准入判定，用户名等会话数据无下游消费方，不进请求上下文。
func (p *Panel) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := p.auth.CheckSession(r.Context(), sessionToken(r)); err != nil {
			p.log.Debug("panel session rejected", "path", r.URL.Path, "reason", err.Error())
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 登录 / 登出 ----

// loginTmpl 解析 embed 的登录页模板（html/template 转义兜底）。
var loginTmpl = template.Must(template.ParseFS(embedded, "web/login.html"))

// renderLogin 输出登录页（status 决定 200/401/429）。
func (p *Panel) renderLogin(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := loginTmpl.Execute(w, loginPageData{Error: errMsg}); err != nil {
		p.log.Error("render login page", "err", err.Error())
	}
}

// clientIP 提取登录防护用的客户端 IP（RemoteAddr 内核来源，不读转发头）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleLoginPage 渲染登录页（匿名可及；已登录直接回面板）。
func (p *Panel) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if sess, err := p.auth.CheckSession(r.Context(), sessionToken(r)); err == nil && sess != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	p.renderLogin(w, http.StatusOK, "")
}

// handleLogin 处理登录表单（SPEC-M1b-c2 §1：登录防护在 auth.Manager——
// per-IP 5 次/分 429、全局 bcrypt ≤2、未知用户 dummy bcrypt）。
// 成功发会话 Cookie 并 302 /；失败按错误类型回 401/429 登录页。
func (p *Panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// 体上限先于解析生效：超限在 ParseForm 内即报错。
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	if err := r.ParseForm(); err != nil || len(r.PostForm) > 16 {
		p.renderLogin(w, http.StatusBadRequest, "请求无效")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if username == "" || password == "" || len(username) > 128 || len(password) > 256 {
		p.renderLogin(w, http.StatusBadRequest, "请求无效")
		return
	}
	token, err := p.auth.Login(r.Context(), clientIP(r), username, password)
	if err != nil {
		p.log.Warn("login failed", "ip", clientIP(r), "user", username,
			"elapsed_ms", time.Since(start).Milliseconds())
		if errors.Is(err, auth.ErrRateLimited) {
			p.renderLogin(w, http.StatusTooManyRequests, err.Error())
			return
		}
		// 统一文案：未知用户/错密码/禁用用户不区分（防枚举）。
		p.renderLogin(w, http.StatusUnauthorized, auth.ErrInvalidCredentials.Error())
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, time.Now().Add(30*24*time.Hour)))
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout 注销当前会话（SPEC §8 验收「登出失效」）：撤销服务端会话 +
// 清除 Cookie + 302 /login。未登录访问同样落到 /login。
func (p *Panel) handleLogout(w http.ResponseWriter, r *http.Request) {
	tok := sessionToken(r)
	if tok != "" {
		if sess, err := p.auth.CheckSession(r.Context(), tok); err == nil && sess != nil {
			if err := p.auth.Logout(r.Context(), sess.TokenHash); err != nil {
				p.log.Error("logout revoke session", "err", err.Error())
			}
		}
	}
	http.SetCookie(w, auth.SessionCookie("", time.Unix(0, 0)))
	http.Redirect(w, r, "/login", http.StatusFound)
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
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(ov); err != nil {
		p.log.Error("panel overview encode", "err", err)
	}
}

// ---- M1d 命令通道（SPEC-M1d §4：面板提交与查询，单管理员 = operator）----

// commandMetaOut 为 command_meta 输出：kind 白名单（含参数槽定义，驱动表单）+
// 节点下发资格（l2_allowed/在线），前端只渲染不做业务计算。
type commandMetaOut struct {
	Kinds []commandKindMeta `json:"kinds"`
	Nodes []commandNodeMeta `json:"nodes"`
}

type commandKindMeta struct {
	Name      string            `json:"name"`
	Platforms []string          `json:"platforms"`
	Slots     []commandSlotMeta `json:"slots"`
	ArgvHint  string            `json:"argv_hint"`
	Required  []string          `json:"required_caps"` // 展示用（OR 组合拍平）
}

type commandSlotMeta struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // unit | int
	Min     int64  `json:"min,omitempty"`
	Max     int64  `json:"max,omitempty"`
	Managed bool   `json:"managed"` // unit 槽须在受管服务集合内（或 l2_extra 扩展）
}

type commandNodeMeta struct {
	Name      string `json:"name"`
	Online    bool   `json:"online"`
	L2Allowed bool   `json:"l2_allowed"`
	OS        string `json:"os"`
}

func (p *Panel) handleCommandMeta(w http.ResponseWriter, r *http.Request) {
	out := commandMetaOut{Kinds: []commandKindMeta{}, Nodes: []commandNodeMeta{}}
	for _, name := range cmdkind.Names() {
		k := cmdkind.Lookup(name)
		km := commandKindMeta{Name: name, Platforms: k.Platforms, Slots: []commandSlotMeta{}}
		for _, s := range k.Slots {
			sm := commandSlotMeta{Name: s.Name, Managed: s.Managed}
			switch s.Kind {
			case cmdkind.SlotUnit:
				sm.Type = "unit"
			default:
				sm.Type = "int"
				sm.Min, sm.Max = s.Min, s.Max
			}
			km.Slots = append(km.Slots, sm)
		}
		km.ArgvHint = strings.Join(k.Template, " ")
		seen := map[string]bool{}
		for _, combo := range k.RequiredCaps {
			for _, c := range combo {
				if !seen[c] {
					seen[c] = true
					km.Required = append(km.Required, c)
				}
			}
		}
		out.Kinds = append(out.Kinds, km)
	}
	nodes, err := p.q.Nodes(r.Context())
	if err == nil {
		for _, n := range nodes {
			out.Nodes = append(out.Nodes, commandNodeMeta{
				Name: n.Name, Online: n.Status == "online", L2Allowed: n.L2Allowed, OS: n.OS,
			})
		}
	}
	writePanelJSON(w, out)
}

func (p *Panel) handleCommandList(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	cmds, err := p.q.Commands(r.Context(), r.URL.Query().Get("node"), r.URL.Query().Get("status"), limit, false)
	if err != nil {
		p.log.Error("panel command list", "err", err)
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	writePanelJSON(w, map[string]any{"commands": cmds})
}

// handleCommandSubmit 处理命令提交表单（SPEC-M1d §7 验收：面板向 mac-mini 发
// ps_snapshot）。面板会话即单管理员 = operator（SPEC §1）；授权在服务端
// cmdsvc.Submit 统一执行（节点在线/l2 白名单/caps/kind 参数槽/配额/幂等）。
// 成功 200 返回 command_id；失败按错误类型映射 400/403/404/409/429（中文提示）。
func (p *Panel) handleCommandSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCommandFormBody)
	if err := r.ParseForm(); err != nil || len(r.PostForm) > 16 {
		http.Error(w, "请求无效", http.StatusBadRequest)
		return
	}
	node := strings.TrimSpace(r.PostFormValue("node"))
	kind := strings.TrimSpace(r.PostFormValue("kind"))
	unit := strings.TrimSpace(r.PostFormValue("unit"))
	n := strings.TrimSpace(r.PostFormValue("n"))
	timeoutStr := strings.TrimSpace(r.PostFormValue("timeout_s"))
	if node == "" || kind == "" {
		http.Error(w, "请求无效：缺少 node 或 kind", http.StatusBadRequest)
		return
	}
	args := map[string]any{}
	if unit != "" {
		args["unit"] = unit
	}
	if n != "" {
		nn, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			http.Error(w, "请求无效：n 须为整数", http.StatusBadRequest)
			return
		}
		args["n"] = nn
	}
	argsJSON, _ := json.Marshal(args)
	var timeoutS int64
	if timeoutStr != "" {
		v, err := strconv.ParseInt(timeoutStr, 10, 64)
		if err != nil {
			http.Error(w, "请求无效：timeout_s 须为整数", http.StatusBadRequest)
			return
		}
		timeoutS = v
	}
	// 提交者归因：会话用户名；面板单管理员按 operator 记 scope（审计不可改写）。
	username := ""
	if sess, err := p.auth.CheckSession(r.Context(), sessionToken(r)); err == nil && sess != nil {
		username = sess.Username
	}
	res, err := cmdsvc.Submit(r.Context(), p.st, p.cfg, &cmdsvc.SubmitRequest{
		NodeName: node, Kind: kind, ArgsJSON: string(argsJSON),
		TimeoutS: timeoutS, CreatedBy: username, Scope: store.ScopeOperator,
	})
	if err != nil {
		p.log.Warn("panel command submit rejected", "user", username, "node", node,
			"kind", kind, "reason", err.Error())
		switch {
		case errors.Is(err, cmdsvc.ErrNodeNotFound):
			http.Error(w, "节点不存在", http.StatusNotFound)
		case errors.Is(err, cmdsvc.ErrNodeOffline):
			http.Error(w, "节点不在线，无法下发", http.StatusConflict)
		case errors.Is(err, cmdsvc.ErrNodeNotAllowed):
			http.Error(w, "该节点未开放任务下发（l2 白名单仅限 mac-mini/windows，云节点禁止）", http.StatusForbidden)
		case errors.Is(err, cmdsvc.ErrCapsUnsupported):
			http.Error(w, "目标节点不支持该命令类型（caps 协商不匹配）", http.StatusBadRequest)
		case errors.Is(err, cmdsvc.ErrBadKind), errors.Is(err, cmdsvc.ErrBadArgs),
			errors.Is(err, cmdsvc.ErrBadTimeout), errors.Is(err, cmdsvc.ErrBadSubmissionKey):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, store.ErrKeyConflict):
			http.Error(w, "submission_key 已被不同参数使用（幂等窗口内）", http.StatusConflict)
		case errors.Is(err, store.ErrQuotaExceeded):
			http.Error(w, "该节点在途命令已达配额（5），请等待完成后再提交", http.StatusTooManyRequests)
		default:
			http.Error(w, "内部错误", http.StatusInternalServerError)
		}
		return
	}
	p.log.Info("AUDIT command submitted",
		"audit", "command", "command_id", res.Command.CommandID, "node", node,
		"kind", kind, "args", string(argsJSON), "created_by", username,
		"scope", store.ScopeOperator, "idempotent", res.Idempotent)
	writePanelJSON(w, map[string]any{
		"command_id": res.Command.CommandID,
		"status":     res.Command.Status,
		"idempotent": res.Idempotent,
	})
}

// writePanelJSON 统一 JSON 输出（no-store/nosniff 由 guard 已设）。
func writePanelJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
