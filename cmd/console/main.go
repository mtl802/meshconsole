// meshconsole 控制台入口。子命令：
//
//	meshconsole            服务模式：HTTPS 监听（默认）
//	meshconsole pki        生成/校验 CA 与服务端证书（幂等，不覆盖已有私钥）
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
	"sync"
	"syscall"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/pki"
	"github.com/mtl802/meshconsole/internal/registry"
	"github.com/mtl802/meshconsole/internal/store"
	"golang.org/x/net/netutil"
)

var version = "dev"

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

// cmdPKI 执行 `meshconsole pki`：按配置幂等生成 pki 目录（CA + 服务端证书），
// 打印服务端证书指纹（供 agent 配置 fingerprint 粘贴）。
func cmdPKI(args []string) int {
	fs := flag.NewFlagSet("pki", flag.ContinueOnError)
	cfgPath := fs.String("config", "console.yaml", "配置文件路径（读取 pki_dir/tailnet_ip/tls_cert/tls_key）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// pki 子命令不强制注册 token（生成证书是部署前置动作，与 token 配置无关）。
	cfg, err := config.LoadConsoleForPKI(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	res, err := pki.Ensure(pki.Config{
		Dir:       cfg.PKIDir,
		TailnetIP: cfg.TailnetIP,
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
	if len(os.Args) >= 2 && os.Args[1] == "pki" {
		os.Exit(cmdPKI(os.Args[2:]))
	}

	cfgPath := flag.String("config", "console.yaml", "配置文件路径")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.LoadConsole(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	log = newLogger(cfg.LogLevel)
	certPath, keyPath := cfg.CertKeyPaths()
	log.Info("meshconsole starting", "version", version, "listen", cfg.Listen,
		"db", cfg.DBPath, "tls_cert", certPath, "tls_key", keyPath,
		"max_connections", cfg.MaxConnections)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 离线巡检与指标保留清理（SPEC §2/§4）。
	go registry.SweepLoop(ctx, st, log, time.Duration(cfg.OfflineAfterS)*time.Second)
	go registry.CleanupLoop(ctx, st, log, cfg.MetricsRetentionDays)

	mux := http.NewServeMux()
	// /healthz：唯一免认证端点，仅返回 ok（DESIGN §7-2）。
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	registry.New(st, log, cfg.RegistrationTokens).RegisterRoutes(mux)

	// 在途 handler 计数：drainHTTP 第三段靠它确认「没有任何 handler 还在碰 DB」。
	var inFlight sync.WaitGroup
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Done()
		mux.ServeHTTP(w, r)
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

	ln, err := listenTLS(cfg)
	if err != nil {
		log.Error("listen", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}
	log.Info("listening", "addr", cfg.Listen, "tls", true)
	// R5 裁决落地（M1b-a）：连接总数上限由 golang.org/x/net/netutil.LimitListener
	// 实现（listenTLS 内，默认 256 可配）——该实现带 done-channel 修复，listener
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

// listenTLS 组装监听链：TCP → LimitListener（连接总数上限，SPEC-M1b-a §1）→
// TLS（console 启动仅加载 pki 证书，不生成；HTTPS 全覆盖，无明文入口）。
// ServerTLSConfig 会把 pki_dir/ca.crt 追加进下发链（确为签发方时），
// 供仅配置指纹的 agent 做完整 x509 验证（R10-#1）。
func listenTLS(cfg *config.Console) (net.Listener, error) {
	certPath, keyPath := cfg.CertKeyPaths()
	tlsCfg, err := pki.ServerTLSConfig(certPath, keyPath, filepath.Join(cfg.PKIDir, pki.CACertFile))
	if err != nil {
		return nil, err
	}
	tcpLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	ln := tls.NewListener(netutil.LimitListener(tcpLn, cfg.MaxConnections), tlsCfg)
	// net/http 服务端没有 TLS 握手超时字段（ReadHeaderTimeout 在握手之后才生效）：
	// 在 Accept 后对未握手连接设总 deadline 防 slowloris 占满连接配额；
	// 握手成功后 http 层会按 ReadHeaderTimeout/WriteTimeout 重设，不受影响。
	return handshakeDeadlineListener{Listener: ln, timeout: 10 * time.Second}, nil
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
