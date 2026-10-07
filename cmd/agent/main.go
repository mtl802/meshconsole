// meshagent 节点代理入口。子命令：
//
//	meshagent register -console <url> -token <注册token> -name <名称>
//	meshagent run [-config <path>]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/mtl802/meshconsole/internal/agent"
	"github.com/mtl802/meshconsole/internal/agent/collect"
	"github.com/mtl802/meshconsole/internal/config"
)

var version = "dev"

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

func usage() {
	fmt.Fprintf(os.Stderr, `meshagent (%s)
用法:
  meshagent register -console <url> -token <注册token> -name <名称> [-role <角色>] [-state <path>]
  meshagent run [-config <path>] [-state <path>]
`, version)
	os.Exit(2)
}

// cmdRegister 执行一次性注册并持久化节点 token（0600 JSON）。
func cmdRegister(args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径（可选，用于读取 role/state_file 等缺省值）")
	console := fs.String("console", "", "控制台地址，如 http://127.0.0.1:7700")
	token := fs.String("token", "", "一次性注册 token")
	name := fs.String("name", "", "节点名称")
	role := fs.String("role", "", "节点角色（默认 node 或配置文件值）")
	statePath := fs.String("state", "", "state 文件路径（默认 ~/.meshagent/state.json）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := newLogger("info")

	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	if *console == "" || *token == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "register 需要 -console、-token、-name")
		return 2
	}
	if *role == "" {
		*role = cfg.Role
	}
	path := *statePath
	if path == "" {
		path = cfg.StateFile
	}

	// 注册请求绑定 ctx + 超时（审查 R1-#10：所有出站请求可取消）。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := agent.Register(ctx, *console, *token, *name, *role, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		log.Error("register failed", "err", err)
		return 1
	}
	if err := agent.SaveState(path, st); err != nil {
		log.Error("save state failed", "err", err)
		return 1
	}
	log.Info("registered", "node_id", st.NodeID, "name", st.Name, "state_file", path)
	fmt.Printf("registered: node_id=%d name=%s state=%s\n", st.NodeID, st.Name, path)
	return 0
}

// cmdRun 前台运行心跳循环；凭本地持久化 token 续接，不重复注册。
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "agent.yaml", "配置文件路径")
	statePath := fs.String("state", "", "state 文件路径（覆盖配置）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		newLogger("error").Error("load config", "err", err)
		return 1
	}
	log := newLogger(cfg.LogLevel)

	path := *statePath
	if path == "" {
		path = cfg.StateFile
	}
	st, err := agent.LoadState(path)
	if err != nil {
		log.Error("load state", "err", err)
		return 1
	}
	consoleURL := cfg.ConsoleURL
	if consoleURL == "" {
		consoleURL = st.ConsoleURL
	}
	st.ConsoleURL = consoleURL
	// state 里记录的 role 优先（注册时的声明），配置仅作缺省。
	if st.Role == "" {
		st.Role = cfg.Role
	}

	log.Info("meshagent starting", "version", version, "console", consoleURL,
		"node_id", st.NodeID, "name", st.Name, "interval_s", cfg.CollectIntervalS)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := &agent.Runner{
		Cfg:     cfg,
		State:   st,
		Version: version,
		Log:     log,
		Collector: collect.NewCollector(cfg.DiskMount,
			time.Duration(cfg.CollectIntervalS)*time.Second),
	}
	if err := runner.Run(ctx); err != nil {
		log.Error("agent run failed", "err", err)
		return 1
	}
	return 0
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "register":
		os.Exit(cmdRegister(os.Args[2:]))
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "-h", "-help", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", os.Args[1])
		usage()
	}
}
