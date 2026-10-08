// Agent 任务采集（SPEC-M1b-c §2.1）：扫描本机进程，匹配已知 AI agent CLI 的
// 运行实例，并对各 agent 的会话目录做 stat（只 stat 不读不解析内容——会话文件
// 格式耦合禁止），随心跳上报为 agent_tasks 数组与 agents.last_activity。
//
// 匹配口径：按进程命令行首 token 的路径基名匹配（`/usr/local/bin/zcode …` 命中
// zcode）。只看首 token 是刻意收紧——子串/全文匹配会把「vim ~/.codex/sessions/x」
// 之类的无辜进程误报为 codex 在跑。解释器包装（`python -m aider`）不在 v1 覆盖
// 范围，漏报如实（宁漏不误）。
//
// 失败语义：进程扫描整体失败返回 error（调用方置 collect_errors 并缺席本字段，
// 不清空 console 侧任务——扫描失败 ≠ 没有任务）；单条目录 stat 失败/缺失仅该
// agent 的 last_activity 缺席（null），不影响任务清单。
package collect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
)

const (
	// TaskScanBudget 为单轮任务采集总预算（ps/tasklist + 目录 stat 全在内），
	// 超预算中断返回 error——心跳循环绝不被拖死（R11-G 同口径）。
	TaskScanBudget = 8 * time.Second
	// MaxTaskCmdLen 为任务 cmd 字段截断长度（SPEC-M1b-c §2.1：200 字符）。
	MaxTaskCmdLen = 200
	// MaxAgentTasks 为单轮上报任务数上限（与 console 侧校验上限一致）。
	MaxAgentTasks = 64
	// MaxSessionEntries 为单会话目录遍历的条目上限（文件+目录合计，R27-#2）：
	// 只数文件会被海量空目录绕过、遍历无界。触顶停止遍历并如实报 Truncated
	//（截断的计数是下界，不冒充完整统计）。
	MaxSessionEntries = 8192
	// TaskScanMaxOutput 为进程清单输出的收集上限。全量 `ps -eo command` 在进程
	// 多的图形工作站上远超服务查询的 64KB 级上限（真机实测 macOS 558 进程
	// ≈150KB，Electron 类应用单条命令行数 KB）——复用小上限会从中途截断、静默
	// 丢掉高 pid 的进程行。放宽到 1MB 保留封顶语义（R10-#5 有界不变），触顶
	// 如实报错（缺席字段 + collect_errors），不假装「没扫到任务」。
	TaskScanMaxOutput = 1 << 20
)

// taskExecRunner 大上限版执行器：与 execRunner 同语义（固定 argv、WaitDelay
// 防孙进程挂管道），仅输出封顶放宽到 TaskScanMaxOutput。
type taskExecRunner struct{}

func (taskExecRunner) Run(ctx context.Context, name string, args ...string) (cmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, errOut := &cappedBuffer{max: TaskScanMaxOutput}, &cappedBuffer{max: TaskScanMaxOutput}
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.WaitDelay = 500 * time.Millisecond
	err := cmd.Run()
	return cmdResult{
		stdout:    out.buf.String(),
		stderr:    errOut.buf.String(),
		truncated: out.truncated || errOut.truncated,
	}, err
}

// AgentTask 为一条运行中的 agent 任务（JSON 对应 console agent_tasks 表行）。
// CPUPct/MemPct 为 nil 表示该维度不可用（Windows tasklist 兜底无此数据，报 null
// 不填 0，与全站 null 语义一致）。CPUPct 保持 ps 原值上报（R27-#1）：多核 %CPU
// 超 100 是 ps 语义（8 核打满 = 800），服务端校验同口径只拒 NaN/Inf/负数。
type AgentTask struct {
	PID       int64    `json:"pid"`
	AgentName string   `json:"agent_name"`
	Cmd       string   `json:"cmd"`
	ElapsedS  int64    `json:"elapsed_s"`
	CPUPct    *float64 `json:"cpu_pct,omitempty"`
	MemPct    *float64 `json:"mem_pct,omitempty"`
	// StartedAt 为 unix 秒（由 etime 反推）；0 = 未知（Windows 兜底无 etime）。
	StartedAt int64 `json:"started_at,omitempty"`
}

// DirActivity 为一个 agent 会话目录的 stat 辅证：目录树内最近 mtime（unix 秒）
// 与文件数。零值不外发——目录缺失/stat 失败的 agent 在 map 中缺席即 null。
// Truncated = 条目（文件+目录）触顶 MaxSessionEntries 截断：Files 是下界而非
// 全树计数，调用方须如实上报不完整状态（collect_errors），不静默。
type DirActivity struct {
	LastActivity int64 `json:"last_activity"`
	Files        int64 `json:"files"`
	Truncated    bool  `json:"truncated"`
}

// knownSessionDirs 内置已知 agent 的会话目录（~ 开头，使用时展开）。仅收录有
// 公认固定位置的 CLI；aider 无固定会话目录，缺省不统计（last_activity null）。
// 用户可用 agent_scan.known: [] + custom 的 session_dir 整体替换。
var knownSessionDirs = map[string]string{
	"zcode":  "~/.zcode/cli/rollout",
	"codex":  "~/.codex/sessions",
	"claude": "~/.claude/projects",
	"gemini": "~/.gemini/tmp",
}

// AgentTaskScanner 扫描本机 agent 任务进程与会话目录活跃度。
// names 取 agentdisc 已知清单（known 默认五项 + config custom 并集，同名 custom
// 优先其 session_dir 声明）。
type AgentTaskScanner struct {
	names       []string
	sessionDirs map[string]string // agent 名 → 已展开的会话目录（空串 = 不统计）

	runner cmdRunner
	// budget/goos/now 可注入（测试）；零值取默认。
	budget time.Duration
	goos   string
	now    func() time.Time
}

// NewAgentTaskScanner 构造。known 为已知 CLI 名清单，custom 为显式声明
// （同名覆盖 known 的会话目录映射，与 agentdisc 合并规则一致；SessionDir 已在
// 配置加载时 ~ 展开）。
func NewAgentTaskScanner(known []string, custom []config.CustomAgent) *AgentTaskScanner {
	cdirs := make([]struct {
		Name       string
		SessionDir string
	}, len(custom))
	for i, c := range custom {
		cdirs[i] = struct {
			Name       string
			SessionDir string
		}{c.Name, c.SessionDir}
	}
	return newAgentTaskScanner(known, cdirs, taskExecRunner{}, runtime.GOOS, time.Now)
}

// newAgentTaskScanner 内部构造（依赖注入形态，单测用）。
func newAgentTaskScanner(
	known []string,
	custom []struct {
		Name       string
		SessionDir string
	},
	runner cmdRunner, goos string, now func() time.Time,
) *AgentTaskScanner {
	seen := map[string]bool{}
	names := make([]string, 0, len(known)+len(custom))
	dirs := map[string]string{}
	for _, n := range known {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	for _, c := range custom {
		if c.Name == "" {
			continue
		}
		if !seen[c.Name] {
			seen[c.Name] = true
			names = append(names, c.Name)
		}
		dirs[c.Name] = c.SessionDir
	}
	// known 名的目录映射仅在 custom 未声明同名时生效（声明优先，与 agentdisc 合并
	// 规则一致）；内置映射带 ~，与 config 路径同口径展开。
	for _, n := range names {
		if _, ok := dirs[n]; !ok {
			dirs[n] = config.ExpandHome(knownSessionDirs[n])
		}
	}
	sort.Strings(names)
	return &AgentTaskScanner{
		names:       names,
		sessionDirs: dirs,
		runner:      runner,
		budget:      TaskScanBudget,
		goos:        goos,
		now:         now,
	}
}

// Enabled 报告是否配置了任何可扫描的 agent 名（known ∪ custom 为空时任务采集
// 无目标，调用方应整体缺席 agent_tasks 字段而非每拍上报空数组）。
func (s *AgentTaskScanner) Enabled() bool { return len(s.names) > 0 }

// Scan 执行一轮采集：返回匹配到的任务清单、各 agent 会话目录活跃度，以及
// truncated 标记（本拍任务数触顶 MaxAgentTasks、清单不完整，R27-#4）。
// 进程扫描失败返回 error（调用方缺席字段 + collect_errors，不清空 console 侧
// 任务——扫描失败 ≠ 没有任务），此时活动度仍然返回（子项独立，目录 stat 有效
// 就如实上报）；会话目录子项失败不报错、该 agent 在返回 map 中缺席。
// ctx 取消（超预算/停机）按 error 返回。
func (s *AgentTaskScanner) Scan(ctx context.Context) ([]AgentTask, map[string]DirActivity, bool, error) {
	bctx := ctx
	if s.budget > 0 {
		var cancel context.CancelFunc
		bctx, cancel = context.WithTimeout(ctx, s.budget)
		defer cancel()
	}
	tasks, truncated, err := s.scanProcesses(bctx)
	// 目录 stat 与进程扫描相互独立：任一方向失败不拖累另一方向。目录遍历同样
	// 挂整轮预算 ctx（R27-#2）——8s 预算对该路径真正生效。
	activity := s.scanActivity(bctx)
	if err != nil {
		return nil, activity, false, err
	}
	return tasks, activity, truncated, nil
}

// scanProcesses 按平台扫描进程清单并匹配 agent 名。返回的 truncated 标记本拍
// 匹配数超过 MaxAgentTasks、清单只保留前 64 条（R27-#4：截断必须可见，不得
// 冒充全量快照——console 侧会全量替换，静默截断会把第 65 个起的任务从面板
// 上无声抹掉）。
func (s *AgentTaskScanner) scanProcesses(ctx context.Context) ([]AgentTask, bool, error) {
	if len(s.names) == 0 {
		return []AgentTask{}, false, nil
	}
	now := s.now().Unix()
	var tasks []AgentTask
	truncated := false
	if s.goos == "windows" {
		// Windows 兜底（SPEC-M1b-c §3：精细化留 M1c+）：tasklist 只有镜像名与
		// pid——cpu/mem/etime/started_at 一律缺席（null/0），cmd 记镜像名。
		res, err := s.runner.Run(ctx, "tasklist", "/FO", "CSV", "/NH")
		if err != nil {
			return nil, false, taskScanErr("tasklist", err, ctx)
		}
		if res.truncated {
			return nil, false, errors.New(truncErr("tasklist output exceeds collection cap; process list incomplete"))
		}
		for _, ln := range nonEmptyLines(res.stdout) {
			fields := splitCSVLine(ln)
			if len(fields) < 2 {
				continue
			}
			image := strings.TrimSuffix(fields[0], ".exe")
			pid, perr := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
			if perr != nil || pid <= 0 {
				continue
			}
			if matchImageName(image, s.names) == "" {
				continue
			}
			// 触顶不 break：剩余行继续解析计数（输出已封顶，代价可忽略），
			// 换取准确的截断判定——清单装不下的任务必须可见（R27-#4）。
			if len(tasks) >= MaxAgentTasks {
				truncated = true
				continue
			}
			tasks = append(tasks, AgentTask{
				PID:       pid,
				AgentName: image,
				Cmd:       trunc(strings.TrimSpace(fields[0]), MaxTaskCmdLen),
			})
		}
		return tasks, truncated, nil
	}
	// unix：ps -eo pid,etime,pcpu,pmem,command。表头行 pid 解析失败自然跳过。
	res, err := s.runner.Run(ctx, "ps", "-eo", "pid,etime,pcpu,pmem,command")
	if err != nil {
		return nil, false, taskScanErr("ps", err, ctx)
	}
	if res.truncated {
		// 触顶即如实报错：截断点之后的进程行已丢失，硬扫会得出「部分任务在跑」
		// 的错误快照并错误清行——缺席字段 + collect_errors 才是诚实语义。
		return nil, false, errors.New(truncErr("ps output exceeds collection cap (1MB); process list incomplete"))
	}
	for _, ln := range nonEmptyLines(res.stdout) {
		// ps 输出按列宽对齐、分隔为不定长空白：Fields 拆前四列，其余原样作
		// 命令行（cmd 为摘要展示，参数间空白归一为单空格不影响可读性）。
		fields := strings.Fields(ln)
		if len(fields) < 5 {
			continue
		}
		pid, perr := strconv.ParseInt(fields[0], 10, 64)
		if perr != nil || pid <= 0 {
			continue // 表头/残行
		}
		cmdline := strings.Join(fields[4:], " ")
		name := matchCommandLine(cmdline, s.names)
		if name == "" {
			continue
		}
		// 触顶不 break（同 Windows 路径）：继续解析计数换准确截断判定。
		if len(tasks) >= MaxAgentTasks {
			truncated = true
			continue
		}
		task := AgentTask{
			PID:       pid,
			AgentName: name,
			Cmd:       trunc(cmdline, MaxTaskCmdLen),
			ElapsedS:  parseEtime(fields[1]),
		}
		if cpu, ok := parsePct(fields[2]); ok {
			task.CPUPct = &cpu
		}
		if mem, ok := parsePct(fields[3]); ok {
			task.MemPct = &mem
		}
		if task.ElapsedS > 0 {
			task.StartedAt = now - task.ElapsedS
		}
		tasks = append(tasks, task)
	}
	return tasks, truncated, nil
}

// scanActivity 逐 agent stat 会话目录（只 stat 不读内容）。目录缺失/无权限/
// 预算耗尽的 agent 在结果中缺席（上层如实报 null）；条目触顶截断的 agent 照常
// 返回（带 Truncated 标记，由调用方如实上报不完整）。
func (s *AgentTaskScanner) scanActivity(ctx context.Context) map[string]DirActivity {
	out := map[string]DirActivity{}
	for name, dir := range s.sessionDirs {
		if dir == "" {
			continue
		}
		if a, ok := statDirTree(ctx, dir); ok {
			out[name] = a
		}
	}
	return out
}

// dirReadBatch 为单目录分批 readdir 的批大小（R29-#1）：每批至多读这么多条目，
// 批间检查 ctx 与条目累计配额。弃用 filepath.WalkDir 的根因在其读取阶段——
// WalkDir 内部对单个目录 os.ReadDir 一次性整读并排序后才逐项回调，超大单目录
// （数十万条目）的读取量发生在回调之前，配额与 ctx 的检查鞭长莫及；分批后
// 单目录的读取量被「条目配额 + 一批」封顶。
const dirReadBatch = 256

// statDirTree 遍历目录树统计文件数与最近 mtime（全程只 lstat——ReadDir 条目的
// Info() 即 lstat，不打开任何文件内容）。只记文件 mtime——目录 mtime 会被建
// 目录/增删文件之外的行为污染，文件时间才是「最近会话活动」的信号。
func statDirTree(ctx context.Context, root string) (DirActivity, bool) {
	return walkDirTree(ctx, root, MaxSessionEntries, dirReadBatch)
}

// walkDirTree 为 statDirTree 的参数化形态（entryCap/batchSize 供单测注入小
// 配额/小批量钉行为，生产值见 statDirTree）。
//
// 遍历为显式栈式 DFS + 分批 readdir（R29-#1 二修）：每个目录 Open 后按
// batchSize 分批 ReadDir，批间检查 ctx.Err()，批内每计一条检查条目累计配额
// ——单目录读取量受「ctx 与配额」双重支配，触顶/超时立即收敛，不无界读盘。
// 条目（文件+目录合计，R27-#2：只数文件挡不住海量空目录的无限遍历）触顶即
// 停止并置 Truncated（截断的 Files 是下界，不冒充完整统计）；ctx 取消/超预算
// 中断返回 ok=false（预算内没数完 = 未知，缺席如实上报，不拿半程结果冒充
// 完整）；根目录不存在/无权限返回 ok=false。
func walkDirTree(ctx context.Context, root string, entryCap int64, batchSize int) (DirActivity, bool) {
	if err := ctx.Err(); err != nil {
		return DirActivity{}, false
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return DirActivity{}, false
	}
	var a DirActivity
	// 根目录自身即一个条目（沿用 WalkDir 计数口径：根是第一个被访问的条目）。
	entries := int64(1)
	if entries >= entryCap {
		a.Truncated = true
		return a, true
	}
	// 待遍历目录栈。子目录在父目录的分批结果里被发现时压栈（发现即已计入
	// 条目配额）。不跟随符号链接——ReadDir 条目的 IsDir() 对 symlink 为
	// false、按文件计，与 WalkDir 口径一致，天然无环。
	dirs := []string{root}
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		f, err := os.Open(dir)
		if err != nil {
			// 单目录打开失败（权限/竞态消失）：跳过该子树，不中断整树
			// （与旧 WalkDir 的 SkipDir 口径一致）。
			continue
		}
		for {
			// 批间检查 ctx（R29-#1）：超时/停机立即终止整树，等价旧口径的
			// 「预算内没数完 = 未知」，绝不带着半程数据返回。批内配额检查
			// 在下方逐条进行，比批间更密。
			if cerr := ctx.Err(); cerr != nil {
				f.Close()
				return DirActivity{}, false
			}
			batch, rerr := f.ReadDir(batchSize)
			for i := range batch {
				e := batch[i]
				if e.IsDir() {
					dirs = append(dirs, filepath.Join(dir, e.Name()))
				} else {
					a.Files++
					// Info() 即 lstat（不打开内容）；取不到 mtime 的条目
					// 照常计数，仅该条时间缺席。
					if fi, ierr := e.Info(); ierr == nil {
						if unix := fi.ModTime().Unix(); unix > a.LastActivity {
							a.LastActivity = unix
						}
					}
				}
				entries++
				// 文件+目录合计触顶即停（R27-#2）：当前条目照常计入，其后
				// 整树放弃并如实报截断——配额先于下一批读取生效，单目录
				// 读取量因此收敛在配额+一批之内。
				if entries >= entryCap {
					a.Truncated = true
					f.Close()
					return a, true
				}
			}
			if rerr != nil {
				// 正常读完（EOF）与罕见读错误（EIO 等）同栈收尾：本目录停止
				// 读取——后者余量未知，按旧 WalkDir 口径放弃剩余条目，已读
				// 部分的统计照常保留，其余子树不受影响。
				f.Close()
				break
			}
		}
	}
	return a, true
}

// matchCommandLine 取命令行首 token 的路径基名匹配 agent 名，命中返回该名。
func matchCommandLine(cmdline string, names []string) string {
	first := strings.Fields(cmdline)
	if len(first) == 0 {
		return ""
	}
	return matchImageName(filepath.Base(first[0]), names)
}

// matchImageName 按可执行名匹配（Windows 镜像名的 .exe 后缀已由调用方去除）。
func matchImageName(base string, names []string) string {
	for _, n := range names {
		if base == n {
			return n
		}
	}
	return ""
}

// parseEtime 解析 ps 的 ELAPSED（[[dd-]hh:]mm:ss）；不可解析返回 0（不编造）。
func parseEtime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	days := int64(0)
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil || d < 0 {
			return 0
		}
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var secs int64
	for _, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v < 0 {
			return 0
		}
		secs = secs*60 + v
	}
	if len(parts) == 2 { // mm:ss
		return days*86400 + secs
	}
	// hh:mm:ss
	hh := secs / 3600
	return days*86400 + hh*3600 + secs%3600
}

// parsePct 解析 ps 的 %CPU/%MEM 列；非法/负值返回 ok=false（调用方报 null）。
func parsePct(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// splitCSVLine 拆 tasklist 的 CSV 行（去引号；内存列的千分位逗号在引号内不受影响）。
func splitCSVLine(ln string) []string {
	fields := strings.Split(ln, ",")
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.Trim(strings.TrimSpace(f), `"`)
	}
	return out
}

// taskScanErr 归一进程扫描失败说明（含超时区分；出错即在本包截断净化，
// 调用方可直接进 collect_errors——不外泄未裁剪的 ps/tasklist 原文）。
func taskScanErr(tool string, err error, ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New(truncErr(tool + ": " + err.Error()))
}

// trunc rune 边界安全截断（agentdisc 同口径）。
func trunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !isRuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
