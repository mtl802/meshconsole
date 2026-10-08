// meshagent 节点代理入口。子命令：
//
//	meshagent register -console <url> -token <注册token> -name <名称>
//	meshagent run [-config <path>]
//
// console 地址强制 https（R10-#1），不存在明文 http 路径。
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
	"github.com/mtl802/meshconsole/internal/agentdisc"
	"github.com/mtl802/meshconsole/internal/config"
)

var (
	// version/commit 由 Makefile ldflags 注入（观察点①②）。
	version = "dev"
	commit  = "none"
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

func usage() {
	fmt.Fprintf(os.Stderr, `meshagent (%s · commit %s)
用法:
  meshagent register -console <url> -token <注册token> -name <名称> [-role <角色>] [-state <path>]
                     [-ca-cert <CA证书路径>] [-fingerprint <SHA-256指纹>]
                     （https 地址必须带 -ca-cert 或 -fingerprint 之一）
  meshagent run [-config <path>] [-state <path>]
  meshagent --version
`, version, commit)
	os.Exit(2)
}

// cmdRegister 执行一次性注册并持久化节点 token（0600 JSON）。
// https 上报必须带 -ca-cert 或 -fingerprint 之一（或配置文件已配）。
func cmdRegister(args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径（可选，用于读取 role/state_file/ca_cert/fingerprint 等缺省值）")
	console := fs.String("console", "", "控制台地址，如 https://127.0.0.1:7700")
	token := fs.String("token", "", "一次性注册 token")
	name := fs.String("name", "", "节点名称")
	role := fs.String("role", "", "节点角色（默认 node 或配置文件值）")
	statePath := fs.String("state", "", "state 文件路径（默认 ~/.meshagent/state.json）")
	caCert := fs.String("ca-cert", "", "控制台 CA 证书 PEM 路径（https 验证；缺省取配置值）")
	fingerprint := fs.String("fingerprint", "", "控制台服务端证书 SHA-256 指纹（https 固定验证；缺省取配置值）")
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
	if *caCert == "" {
		*caCert = cfg.CACert
	}
	if *fingerprint == "" {
		*fingerprint = cfg.Fingerprint
	}
	client, err := agent.NewHTTPClient(*console, *caCert, *fingerprint)
	if err != nil {
		log.Error("build http client", "err", err)
		return 1
	}

	// 注册请求绑定 ctx + 超时（审查 R1-#10：所有出站请求可取消）。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := agent.Register(ctx, client, *console, *token, *name, *role, runtime.GOOS, runtime.GOARCH)
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

	// 上报客户端：https 时强制 TLS 固定验证（ca_cert 链和/或 fingerprint），
	// 两者皆空在此处直接启动失败（不提供跳过验证的选项）。
	client, err := agent.NewHTTPClient(consoleURL, cfg.CACert, cfg.Fingerprint)
	if err != nil {
		log.Error("build http client", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scanCfg := cfg.ScanCfg()
	runner := &agent.Runner{
		Cfg:     cfg,
		State:   st,
		Version: version,
		Log:     log,
		Collector: collect.NewCollector(cfg.DiskMount,
			time.Duration(cfg.CollectIntervalS)*time.Second),
		// 服务状态查询（配置未声明时 CheckAll 不执行、心跳不带 services 字段）；
		// docker 型走配置的 docker_bin（默认 "docker"，生产建议只读 helper）。
		Services: collect.NewServiceChecker(cfg.Services, cfg.DockerBin),
		// AI agent 发现（默认已知清单 + 自定义声明，低频 5 分钟）。
		Discover: agentdisc.New(scanCfg.KnownList(), scanCfg.Custom, scanCfg.Services),
		// agent 任务采集（SPEC-M1b-c §2.1）：进程扫描 + 会话目录 stat，每 15s
		// 随心跳上报；无任何 agent 名（known 清空且无 custom）时自动缺席字段。
		TaskScan: collect.NewAgentTaskScanner(scanCfg.KnownList(), scanCfg.Custom),
		Client:   client,
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
	case "--version", "-version", "version":
		// 观察点①：输出语义版本 + commit（Makefile ldflags 注入）。
		fmt.Printf("meshagent %s (commit %s)\n", version, commit)
		return
	case "-h", "-help", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", os.Args[1])
		usage()
	}
}
