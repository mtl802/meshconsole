package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
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
// M1b-c 新增：TaskScan 每 15s 随心跳采集 agent 任务（agent_tasks）与会话目录
// 活跃度（合并进 agents 条目的 last_activity）。
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
	// TaskScan 为 agent 任务采集器；nil 表示心跳不带 agent_tasks 字段。
	TaskScan *collect.AgentTaskScanner
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

// heartbeatExtras 组装心跳扩展字段（services/agents/agent_tasks），保持 client
// 层协议无关。apply 可向 snap.Errors 追加采集说明（agent_tasks 进程扫描失败等），
// 由 reportOnce 统一随 collect_errors 上报。
type heartbeatExtras struct {
	services bool
	checker  *collect.ServiceChecker
	reports  *atomic.Pointer[[]agentdisc.Report]
	tasks    *collect.AgentTaskScanner

	// tasksOut/activity/tasksErr/tasksTrunc 为本 beat 的任务采集结果（reportOnce
	// 预取，因失败说明须并进 collect_errors 后才有完整的 metrics 快照）。
	tasksOut []collect.AgentTask
	activity map[string]collect.DirActivity
	tasksErr error
	// tasksTrunc 为 true 表示本拍任务清单触顶 64 条被截断（R27-#4：随心跳
	// 显式上报 agent_tasks_truncated，console 落库并在面板标注「清单不完整」）。
	tasksTrunc bool
}

func (e *heartbeatExtras) apply(ctx context.Context, snap *collect.Snapshot, body map[string]any) {
	if e == nil {
		return
	}
	if e.services && e.checker != nil {
		// 配置声明过 services（哪怕显式空列表）即每次上报：空列表驱动 console
		// 侧把消失的服务转 stale（全量替换语义）。CheckAll 对空清单返回 nil，
		// 必须转成空数组（R17-#3 注释勘误，与 R11-A 三态语义对齐）：console
		// 侧「字段缺席」才是无变化；显式 null 是协议违规、一律 400——发 null
		// 不会被视为缺席，而是整条心跳被拒。
		svcs := e.checker.CheckAll(ctx)
		if svcs == nil {
			svcs = []collect.ServiceStatus{}
		}
		body["services"] = svcs
	}
	if e.reports != nil {
		if p := e.reports.Load(); p != nil {
			reports := *p
			// M1b-c：会话目录活跃度合并进 agents 条目（last_activity），以心跳
			// 频率（15s）刷新，不等 5 分钟发现扫描。仅当本 beat 拿到了活跃度才
			// 替换；扫描器缺席/未产出时按原样上报（字段缺席，console 侧 null）。
			if act := e.activity; act != nil {
				reports = mergeActivity(reports, act)
				// 会话目录遍历触顶截断（R27-#2）：截断的 files 计数是下界而非
				// 全树统计，如实进 collect_errors 不静默（与 ps 输出触顶同口径）；
				// 目录缺失/预算耗尽本就按缺席上报，不在此列。
				var truncated []string
				for name, a := range act {
					if a.Truncated {
						truncated = append(truncated, name)
					}
				}
				if len(truncated) > 0 {
					sort.Strings(truncated)
					snap.Errors["agent_activity"] = fmt.Sprintf(
						"session dir scan truncated at %d entries: %s",
						collect.MaxSessionEntries, strings.Join(truncated, ", "))
				}
			}
			body["agents"] = reports
		}
	}
	if e.tasks != nil {
		// 三态语义沿用（SPEC-M1b-c §2.1）：进程扫描成功即上报数组——空数组 =
		// 清空该节点任务；扫描失败（ps/tasklist 不可用、超预算）缺席字段 +
		// collect_errors 说明，绝不能发空数组把「不知道」伪装成「没有任务」。
		if e.tasksErr != nil {
			snap.Errors["agent_tasks"] = e.tasksErr.Error()
		} else if e.tasksOut != nil {
			body["agent_tasks"] = e.tasksOut
			// 清单触顶截断（R27-#4）：显式标记本拍清单不完整；未截断不带该
			// 字段（缺席 = 完整清单，console 侧默认 false）。
			if e.tasksTrunc {
				body["agent_tasks_truncated"] = true
			}
		}
	}
}

// mergeActivity 把会话目录活跃度合入发现报告副本（缓存数组不被原地修改）。
func mergeActivity(reports []agentdisc.Report, act map[string]collect.DirActivity) []agentdisc.Report {
	out := make([]agentdisc.Report, len(reports))
	for i, r := range reports {
		if a, ok := act[r.Name]; ok && a.LastActivity > 0 {
			mt, files := a.LastActivity, a.Files
			r.LastActivity = &mt
			r.SessionFiles = &files
		}
		out[i] = r
	}
	return out
}

func (r *Runner) reportOnce(ctx context.Context, client *http.Client) error {
	extras := &heartbeatExtras{reports: &r.agentReports}
	if r.Cfg.Services != nil {
		extras.services = true
		extras.checker = r.Services
	}
	if r.TaskScan != nil && r.TaskScan.Enabled() {
		extras.tasks = r.TaskScan
		// 任务采集在组装前执行（失败说明要进 collect_errors，须先于 body 编码）。
		tasks, act, trunc, err := r.TaskScan.Scan(ctx)
		if err == nil && tasks == nil {
			// 零匹配必须转显式空数组：nil 会序列化成 null（协议违规 400），
			// 且 console 侧三态里「空数组 = 清空」正是这里要表达的语义。
			tasks = []collect.AgentTask{}
		}
		extras.tasksOut = tasks
		extras.activity = act
		extras.tasksErr = err
		extras.tasksTrunc = trunc
	}
	return reportOnce(ctx, client, r.State.ConsoleURL, r.State.NodeToken, r.State.Name, r.Version, r.Collector, extras)
}

// jitter 返回 [0.5d, d) 的随机抖动时长（上限即 d 本身，封顶后不超 backoffMax）。
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Float64()*float64(d/2))
}
