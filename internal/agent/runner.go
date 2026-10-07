package agent

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mtl802/meshconsole/internal/agent/collect"
	"github.com/mtl802/meshconsole/internal/agentdisc"
	"github.com/mtl802/meshconsole/internal/config"
)

const (
	backoffBase = time.Second
	backoffMax  = 60 * time.Second

	// agentScanInterval 为 AI agent 发现扫描周期（DESIGN §4.2-A：低频，每 5 分钟）。
	agentScanInterval = 5 * time.Minute
	// agentScanBudget 为单轮扫描总预算（5 个已知 CLI × 5s 版本超时 + 端口探测余量）。
	agentScanBudget = 60 * time.Second
)

// Runner 前台心跳循环：每 CollectIntervalS 采集上报一次；
// 上报失败按指数退避重试（1s→2s→…→60s 封顶，带抖动），成功后回到正常间隔。
// M1b-a 新增：Services 每 15s 随心跳查询上报；Discover 每 5 分钟扫描一次，
// 结果缓存并在后续心跳携带（首轮扫描完成前心跳不带 agents 字段）。
type Runner struct {
	Cfg       *config.Agent
	State     *State
	Version   string
	Log       *slog.Logger
	Collector *collect.Collector
	// Services 为受管服务状态查询器；nil 表示配置未声明服务（心跳不带 services 字段）。
	Services *collect.ServiceChecker
	// Discover 为 AI agent 发现扫描器；nil 表示心跳不带 agents 字段。
	Discover *agentdisc.Scanner
	// Client 为上报 HTTP 客户端（https 时已带固定验证）；nil 时用默认客户端。
	Client *http.Client

	// agentReports 缓存最近一轮扫描结果；nil = 尚未完成首轮扫描。
	agentReports atomic.Pointer[[]agentdisc.Report]
}

// Run 阻塞运行直至 ctx 取消。
func (r *Runner) Run(ctx context.Context) error {
	// CPU 采样异步进行，不阻塞心跳循环（SPEC §2）。
	r.Collector.Start(ctx)

	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	if r.Discover != nil {
		go r.discoverLoop(ctx)
	}
	interval := time.Duration(r.Cfg.CollectIntervalS) * time.Second
	backoff := time.Duration(0)
	beat := 0

	for {
		err := r.reportOnce(ctx, client)
		var next time.Duration
		if err == nil {
			beat++
			if backoff != 0 {
				r.Log.Info("report recovered, resuming normal interval", "interval", interval.String())
			}
			backoff = 0
			next = interval
		} else {
			if backoff == 0 {
				backoff = backoffBase
			} else {
				backoff *= 2
				if backoff > backoffMax {
					backoff = backoffMax
				}
			}
			next = jitter(backoff)
			if next > backoffMax {
				next = backoffMax // 抖动封顶：重试间隔永不超 60s（审查 R1-#10）
			}
			r.Log.Error("report failed, backing off", "err", err, "retry_in", next.String())
		}

		select {
		case <-ctx.Done():
			r.Log.Info("agent stopping", "heartbeats_sent", beat)
			return nil
		case <-time.After(next):
		}
	}
}

// discoverLoop 低频扫描 AI agent：启动即扫一轮，此后每 5 分钟一轮；
// 结果原子换入（读取方永远看到完整一致的一轮结果）。
func (r *Runner) discoverLoop(ctx context.Context) {
	scan := func() {
		sctx, cancel := context.WithTimeout(ctx, agentScanBudget)
		defer cancel()
		reports := r.Discover.Scan(sctx)
		r.agentReports.Store(&reports)
		r.Log.Info("agent scan done", "discovered", len(reports))
	}
	scan()
	t := time.NewTicker(agentScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan()
		}
	}
}

// heartbeatExtras 组装心跳扩展字段（services/agents），保持 client 层协议无关。
type heartbeatExtras struct {
	services bool
	checker  *collect.ServiceChecker
	reports  *atomic.Pointer[[]agentdisc.Report]
}

func (e *heartbeatExtras) apply(ctx context.Context, body map[string]any) {
	if e == nil {
		return
	}
	if e.services && e.checker != nil {
		// 配置声明过 services（哪怕显式空列表）即每次上报：空列表驱动 console
		// 侧把消失的服务转 stale（全量替换语义）。CheckAll 对空清单返回 nil，
		// 必须转成空数组——null 会被 console 视作「字段缺席、无变化」。
		svcs := e.checker.CheckAll(ctx)
		if svcs == nil {
			svcs = []collect.ServiceStatus{}
		}
		body["services"] = svcs
	}
	if e.reports != nil {
		if p := e.reports.Load(); p != nil {
			body["agents"] = *p
		}
	}
}

func (r *Runner) reportOnce(ctx context.Context, client *http.Client) error {
	extras := &heartbeatExtras{reports: &r.agentReports}
	if r.Cfg.Services != nil {
		extras.services = true
		extras.checker = r.Services
	}
	return reportOnce(ctx, client, r.State.ConsoleURL, r.State.NodeToken, r.State.Name, r.Version, r.Collector, extras)
}

// jitter 返回 [0.5d, d) 的随机抖动时长（上限即 d 本身，封顶后不超 backoffMax）。
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Float64()*float64(d/2))
}
