// meshconsole 控制台入口：加载配置 → 打开 SQLite → 启动 agent API 与后台巡检/清理。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/registry"
	"github.com/mtl802/meshconsole/internal/store"
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

// drainHTTP 优雅退出三段式（R3-#5 → R5 裁决收紧）：① Shutdown 排空在途请求
// （宽限 35s）→ ② 超时则 srv.Close() 强断连接，卡住的 handler 随连接断开快速
// 返回 → ③ WaitGroup 等全部 handler 返回（上限 30s）。等待归零后才返回——
// 此后不会再有 handler 碰数据库，调用方才允许停后台循环、关库；极端情形 30s
// 仍不归零：记 ERROR 留痕后返回退出，此为明示接受的最坏情形（截断残余风险）。
func drainHTTP(log *slog.Logger, srv *http.Server, inFlight *sync.WaitGroup) {
	log.Info("shutting down: draining http connections")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
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
	case <-time.After(handlerWaitCap):
		// 明示接受的最坏情形：强断后 handler 仍未全部返回即退出，ERROR 留痕备查。
		log.Error("handler 未排空，存在截断残余风险", "cap", handlerWaitCap.String())
	}
}

func main() {
	cfgPath := flag.String("config", "console.yaml", "配置文件路径")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.LoadConsole(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	log = newLogger(cfg.LogLevel)
	log.Info("meshconsole starting", "version", version, "listen", cfg.Listen, "db", cfg.DBPath)

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

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}
	log.Info("listening", "addr", cfg.Listen)
	// R5 裁决：连接总数上限（原 limitListener/limitConn，256）整体移除——实际规模
	// 3 节点+1 客户端无现实意义，防御过度是设计债；连接限额挪 M1b 与 TLS 一并实现，
	// 请求并发防线保留在 handler 层 limitConcurrent(64)（internal/registry）。
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
