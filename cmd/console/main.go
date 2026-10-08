// meshconsole 控制台入口。子命令：
//
//	meshconsole            服务模式：HTTPS 监听（agent API + 面板 + 公网 MCP）
//	meshconsole mcp        MCP server（stdio 传输，只读工具，零网络暴露）
//	meshconsole pki        生成/校验 CA 与服务端证书（幂等，不覆盖已有私钥）
//	meshconsole user       账号管理（add/passwd/disable/enable，见 usercmd.go）
//	meshconsole token      MCP API token 管理（create/list/revoke，见 usercmd.go）
//	meshconsole --version  打印语义版本与 commit
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mtl802/meshconsole/internal/auth"
	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/headscale"
	"github.com/mtl802/meshconsole/internal/mcpserver"
	"github.com/mtl802/meshconsole/internal/meshview"
	"github.com/mtl802/meshconsole/internal/panel"
	"github.com/mtl802/meshconsole/internal/pki"
	"github.com/mtl802/meshconsole/internal/registry"
	"github.com/mtl802/meshconsole/internal/store"
	"golang.org/x/net/netutil"
)

var (
	// version/commit 由 Makefile ldflags 注入（观察点②）：-X main.version、
	// -X main.commit。未注入时 version=dev、commit=none。
	version = "dev"
	commit  = "none"
)

const (
	// shutdownGrace 为排空在途请求的宽限期：读超时 30s + 余量（R3-#5）。
	shutdownGrace = 35 * time.Second
	// handlerWaitCap 为强断之后等待全部 handler 返回（WaitGroup 归零）的上限
	// （R5 裁决收紧为 30s；归零才允许继续关库，超时按明示接受的最坏情形退出）。
	handlerWaitCap = 30 * time.Second
)

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// stderrLogger 供 MCP/pki 等子命令使用：stdout 是协议/数据通道，日志只能走
// stderr（MCP stdio 传输下日志进 stdout 会污染 JSON-RPC 帧）。
func stderrLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

// printVersion 输出语义版本 + commit（观察点①：--version 子命令）。
func printVersion(w interface{ Write([]byte) (int, error) }, bin string) {
	fmt.Fprintf(w, "%s %s (commit %s)\n", bin, version, commit)
}

func usage() {
	fmt.Fprintf(os.Stderr, `meshconsole (%s · commit %s)
用法:
  meshconsole [-config <path>]       服务模式：HTTPS 监听（agent API + 面板 + 公网 MCP）
  meshconsole mcp [-config <path>]   MCP server：stdio 传输，六个只读工具，零网络暴露
  meshconsole pki [-config <path>]   生成/校验 CA 与服务端证书（幂等，不覆盖私钥）
  meshconsole user <add|passwd|disable|enable> <用户名> [-config <path>]
                                     账号管理（交互口令；改密/禁用撤销全部会话与 token）
  meshconsole token <create|list|revoke> ... [-config <path>]
                                     MCP API token 管理（明文仅显示一次）
  meshconsole --version              打印语义版本与 commit
`, version, commit)
	os.Exit(2)
}

// cmdVersion 处理 --version/-version/version（观察点①）。
func cmdVersion() int {
	printVersion(os.Stdout, "meshconsole")
	return 0
}

// configFlag 从子命令参数里提取 -config 值（user/token 子命令先于各自 flag
// 解析打开库，需要提前拿到配置路径；缺省与服务模式一致 console.yaml）。
func configFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-config" || args[i] == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(args[i], "-config="), strings.HasPrefix(args[i], "--config="):
			if _, v, ok := strings.Cut(args[i], "="); ok {
				return v
			}
		}
	}
	return "console.yaml"
}

// cmdMCP 执行 `meshconsole mcp`：只读 MCP server（stdio）。
// 复用 console.yaml（只读 db_path 等字段，不强制注册 token——stdio 本机进程
// 模型无需 TLS/token，SPEC-M1b-b §2）；库以只读模式打开（mode=ro +
// query_only=1），不存在任何写路径。
func cmdMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: meshconsole mcp [-config <path>]\n（六个只读工具: list_nodes / get_node / list_services / list_agents / list_agent_tasks / get_mesh_status；日志走 stderr，stdout 为 JSON-RPC 通道）\n")
		fs.PrintDefaults()
	}
	cfgPath := fs.String("config", "console.yaml", "配置文件路径（读取 db_path）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := stderrLogger()
	cfg, err := config.LoadConsoleForPKI(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	st, err := store.OpenReadOnly(cfg.DBPath)
	if err != nil {
		log.Error("open store read-only", "db", cfg.DBPath, "err", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("mcp server starting (stdio)", "version", version, "commit", commit,
		"db", cfg.DBPath, "read_only", true)
	if err := mcpserver.New(st, version, nil).Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Error("mcp server", "err", err)
		return 1
	}
	log.Info("mcp server stopped")
	return 0
}

// cmdPKI 执行 `meshconsole pki`：按配置幂等生成 pki 目录（CA + 服务端证书），
// 打印服务端证书指纹（供 agent 配置 fingerprint 粘贴）。
func cmdPKI(args []string) int {
	fs := flag.NewFlagSet("pki", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: meshconsole pki [-config <path>]\n（幂等生成 CA + 服务端证书到 pki_dir；已存在则不覆盖，输出 SHA-256 指纹供 agent 配置）\n")
		fs.PrintDefaults()
	}
	cfgPath := fs.String("config", "console.yaml", "配置文件路径（读取 pki_dir/tailnet_ip/tls_cert/tls_key）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := stderrLogger()
	// pki 子命令不强制注册 token（生成证书是部署前置动作，与 token 配置无关）。
	cfg, err := config.LoadConsoleForPKI(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	res, err := pki.Ensure(pki.Config{
		Dir:       cfg.PKIDir,
		TailnetIP: cfg.TailnetIP,
		ExtraSANs: cfg.TLSExtraSANs,
	})
	if err != nil {
		log.Error("pki ensure", "err", err)
		return 1
	}
	if res.CAGenerated {
		log.Info("CA generated", "path", res.CACertPath)
	} else {
		log.Info("CA kept (idempotent)", "path", res.CACertPath)
	}
	if res.ServerCertRenewed {
		log.Info("server cert issued", "path", res.ServerCertPath, "key", res.ServerKeyPath)
	} else {
		log.Info("server cert kept (idempotent)", "path", res.ServerCertPath)
	}
	fmt.Printf("ca_cert:     %s\nserver_cert: %s\nserver_key:  %s\nfingerprint(sha256): %s\n",
		res.CACertPath, res.ServerCertPath, res.ServerKeyPath, res.Fingerprint)
	return 0
}

// drainHTTP 优雅退出三段式（R3-#5 → R5 裁决收紧）：① Shutdown 排空在途请求
// （宽限 35s）→ ② 超时则 srv.Close() 强断连接，卡住的 handler 随连接断开快速
// 返回 → ③ WaitGroup 等全部 handler 返回（上限 30s）。等待归零后才返回——
// 此后不会再有 handler 碰数据库，调用方才允许停后台循环、关库；极端情形 30s
// 仍不归零：记 ERROR 留痕后返回退出，此为明示接受的最坏情形（截断残余风险）。
func drainHTTP(log *slog.Logger, srv *http.Server, inFlight *sync.WaitGroup) {
	drainHTTPSeq(log, srv, inFlight, shutdownGrace, handlerWaitCap)
}

// drainHTTPSeq 为 drainHTTP 的时长可注入形态（R15-#4 退出链测试用，对齐 R11-G
// budget 注入先例）：序列与生产退出路径完全同款，仅宽限/等待上限可替换。
func drainHTTPSeq(log *slog.Logger, srv *http.Server, inFlight *sync.WaitGroup, grace, waitCap time.Duration) {
	log.Info("shutting down: draining http connections")
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("http shutdown timed out, forcing close", "err", err)
		if cerr := srv.Close(); cerr != nil {
			log.Warn("http close", "err", cerr)
		}
	}
	done := make(chan struct{})
	go func() {
		inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitCap):
		// 明示接受的最坏情形：强断后 handler 仍未全部返回即退出，ERROR 留痕备查。
		log.Error("handler 未排空，存在截断残余风险", "cap", waitCap.String())
	}
}

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "pki":
			os.Exit(cmdPKI(os.Args[2:]))
		case "mcp":
			os.Exit(cmdMCP(os.Args[2:]))
		case "user":
			os.Exit(cmdUser(os.Args[2:], configFlag(os.Args[2:]), readPasswordTwice))
		case "token":
			os.Exit(cmdToken(os.Args[2:], configFlag(os.Args[2:])))
		case "--version", "-version", "version":
			os.Exit(cmdVersion())
		case "-h", "-help", "--help", "help":
			usage()
		}
	}

	cfgPath := flag.String("config", "console.yaml", "配置文件路径")
	flag.Usage = usage
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.LoadConsole(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	log = newLogger(cfg.LogLevel)
	certPath, keyPath := cfg.CertKeyPaths()
	log.Info("meshconsole starting", "version", version, "commit", commit, "listen", cfg.Listen,
		"db", cfg.DBPath, "tls_cert", certPath, "tls_key", keyPath,
		"max_connections", cfg.MaxConnections, "headscale", cfg.Headscale != nil)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 离线巡检与指标保留清理（SPEC §2/§4）+ 会话过期清理（M1b-c2：sessions
	// 表随登录/登出增删，过期行由本循环低频回收）。
	go registry.SweepLoop(ctx, st, log, time.Duration(cfg.OfflineAfterS)*time.Second)
	go registry.CleanupLoop(ctx, st, log, cfg.MetricsRetentionDays)
	go auth.SessionCleanupLoop(ctx, st, log, 10*time.Minute)

	// Headscale 集成（M1b-b，只读拉取）：配置无 headscale 段 = 禁用（INFO 一次）。
	if cfg.Headscale != nil {
		fetcher := headscale.NewFetcher(headscale.NewClient(cfg.Headscale.URL, cfg.Headscale.APIKey), st)
		go fetcher.Run(ctx, time.Duration(cfg.Headscale.IntervalS)*time.Second, log)
	} else {
		log.Info("headscale integration disabled", "reason", "no headscale section in config")
	}

	// 账号会话管理（M1b-c2 §1）：认证在所有网络形态始终开启，无开关。
	authMgr := auth.New(st, log)

	mux := http.NewServeMux()
	// /healthz：唯一免认证端点，仅返回 ok（DESIGN §7-2；SPEC-M1b-c2 §1 豁免清单）。
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte("ok"))
	})
	// Web 面板（M1b-b + M1b-c2）：会话守卫 + 登录/登出 + Host/Origin 校验
	// （白名单来自 panel_allowed_hosts）+ overview 并发闸 8。
	panel.New(meshview.New(st), log, authMgr, cfg.AllowedHosts()).RegisterRoutes(mux)
	// agent API（M1b-a + M1b-c2 §5）：来源 CIDR 收敛（agent_allowed_cidrs）
	// + 一次性注册 token / 节点 token 认证。
	registry.New(st, log, cfg.RegistrationTokens, cfg.AgentCIDRs()).RegisterRoutes(mux)
	// MCP 公网 HTTP（M1b-c2 §2）：Bearer api_token 认证 + 独立并发闸 4 +
	// JSON 同步响应模式（SDK Stateless）。stdio MCP（子命令）不受影响。
	mux.Handle("/mcp", mcpserver.NewHTTPHandler(st, version, authMgr))

	// 在途 handler 计数：drainHTTP 第三段靠它确认「没有任何 handler 还在碰 DB」。
	var inFlight sync.WaitGroup
	// 全局安全头（R33-#4，SPEC §7「全链 no-store/nosniff」）：包装根 handler 的
	// ResponseWriter，在 WriteHeader 拦截点统一落头——mux 默认 404/405、agent
	// API 各拒绝路径（401/403/413/503）与全部业务响应一律覆盖，无遗漏路径；
	// agent 仅按状态码分支不解析响应头（SPEC §3 已核实），兼容性不回退。
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Done()
		mux.ServeHTTP(&secureHeaderWriter{ResponseWriter: w}, r)
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 13, // 8KB：认证头之外不需要更大的请求头
	}

	// 优雅退出（审查 R1-#5 → R3-#5 收紧 → R5 裁决）：收到信号 → drainHTTP 三段式
	// （Shutdown 35s → 超时强断 → 等 handler 全部归零，上限 30s）之后，主流程才
	// 停后台循环、关 SQLite，保证在途写库不丢。
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		drainHTTP(log, srv, &inFlight)
		close(shutdownDone)
	}()

	tcpLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}
	// 公网模式启动门槛（SPEC-M1b-c2 §4）：按实际绑定地址（listener 解析结果）
	// 判定——非 {127/8, ::1（含 IPv4-mapped 规范化，IsLoopback 覆盖）} 即公网
	// 模式；0.0.0.0/::/空 host 绑定结果为非回环。任一检查失败拒绝启动。
	if err := publicModeGate(tcpLn.Addr(), cfg, *cfgPath, st, log); err != nil {
		tcpLn.Close()
		log.Error("startup gate failed", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}
	ln := tlsWrap(cfg, tcpLn)
	log.Info("listening", "addr", cfg.Listen, "tls", true,
		"mode", listenMode(tcpLn.Addr()))
	// R5 裁决落地（M1b-a）：连接总数上限由 golang.org/x/net/netutil.LimitListener
	// 实现（tlsWrap 内，默认 256 可配）——该实现带 done-channel 修复，listener
	// Close 时阻塞中的 acquire 立即返回，与 Shutdown 无互锁；超额连接在内核
	// accept 队列排队而非被拒。请求并发防线保留在 handler 层 limitConcurrent(64)。
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// 失败路径同样先让在途 handler 收尾，再关库退出。
		drainHTTP(log, srv, &inFlight)
		stop()
		st.Close()
		log.Error("http server", "err", err)
		os.Exit(1)
	}
	<-shutdownDone // Shutdown→(强断)→handler 全部返回之后，才允许停循环/关库
	stop()         // 停离线巡检/保留清理循环；DB.Close 自身会等待在途查询结束
	log.Info("stopped")
}

// tlsWrap 组装 TLS 包装链：LimitListener（连接总数上限，SPEC-M1b-a §1）→ TLS
// （console 启动仅加载 pki 证书，不生成；HTTPS 全覆盖，无明文入口）。
// ServerTLSConfig 会把 pki_dir/ca.crt 追加进下发链（确为签发方时），
// 供仅配置指纹的 agent 做完整 x509 验证（R10-#1）。在 TCP listen 与公网门槛
// 检查之后调用（门槛失败时不做任何 TLS/证书加载）。
func tlsWrap(cfg *config.Console, tcpLn net.Listener) net.Listener {
	certPath, keyPath := cfg.CertKeyPaths()
	tlsCfg, err := pki.ServerTLSConfig(certPath, keyPath, filepath.Join(cfg.PKIDir, pki.CACertFile))
	if err != nil {
		tcpLn.Close()
		logFatal("load tls config", err)
	}
	ln := tls.NewListener(netutil.LimitListener(tcpLn, cfg.MaxConnections), tlsCfg)
	return handshakeDeadlineListener{Listener: ln, timeout: 10 * time.Second}
}

// listenMode 报告监听形态（日志用）：回环 / 公网。
func listenMode(addr net.Addr) string {
	if isLoopbackAddr(addr) {
		return "loopback"
	}
	return "public"
}

// isLoopbackAddr 判定监听地址是否回环：127/8、::1，IPv4-mapped 先规范化
// （net.IP.IsLoopback 内建该语义）；0.0.0.0/::/空 host 均为非回环。
func isLoopbackAddr(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	return tcp.IP != nil && tcp.IP.IsLoopback()
}

// publicModeGate 公网模式启动门槛（SPEC-M1b-c2 §4）：回环监听直接放行；
// 非回环（公网形态）逐项检查，任一失败返回指明原因的错误（拒绝启动）：
// ①存在 ≥1 个 enabled 用户；②config 文件普通文件且 0600 且属运行用户
// （Windows 仅普通文件，ACL 等价约束）；③panel_allowed_hosts 非空（合法性
// 与显式空数组语义已在 config 加载期校验并规范化，此处兜底复核）。
// 认证开关不在此列——认证在所有网络形态始终开启（无「内网免登录」路径），
// 非回环绑定只是额外的启动门槛，不是认证开关。
func publicModeGate(addr net.Addr, cfg *config.Console, cfgPath string, st *store.Store, log *slog.Logger) error {
	if isLoopbackAddr(addr) {
		return nil
	}
	log.Warn("public network mode detected: startup gate applies",
		"addr", addr.String(), "checks", "enabled_user/config_perm/allowed_hosts")
	// ① 至少一个启用用户——否则公网形态无人可登录，面板全 302、MCP 全 401。
	n, err := st.CountEnabledUsers(context.Background())
	if err != nil {
		return fmt.Errorf("统计启用用户失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("公网模式启动被拒绝：尚无任何启用用户——先执行 `meshconsole user add <用户名>` 创建账号（认证始终开启，公网形态必须有可登录用户）")
	}
	// ② config 文件本身的安全约束（含账号口令哈希的部署不该以宽松权限暴露）。
	if err := config.CheckConfigFileSecurity(cfgPath); err != nil {
		return err
	}
	// ③ Host 白名单非空兜底（正常路径由 config 加载校验保证）。
	if len(cfg.AllowedHosts()) == 0 {
		return fmt.Errorf("公网模式启动被拒绝：panel_allowed_hosts 解析结果为空")
	}
	return nil
}

// logFatal 为 tlsWrap 内 fatal 退出（保持调用点简洁）。
func logFatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}

// secureHeaderWriter 为全局安全头包装（R33-#4）：WriteHeader 拦截点统一写入
// X-Content-Type-Options: nosniff 与 Cache-Control: no-store，覆盖 mux 默认
// 错误响应与全部 handler 的错误/成功路径；隐式 200（直接 Write 不调
// WriteHeader）同覆盖。头为强制覆写（非缺失才补）——与既有各 handler 自行
// 设置的同名头一致，保证 SPEC §7 字面口径成立。
type secureHeaderWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *secureHeaderWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *secureHeaderWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush 透传（MCP SDK 的响应刷新路径需要）。
func (w *secureHeaderWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type handshakeDeadlineListener struct {
	net.Listener
	timeout time.Duration
}

func (l handshakeDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*tls.Conn); ok {
		_ = tc.SetDeadline(time.Now().Add(l.timeout))
	}
	return c, nil
}
