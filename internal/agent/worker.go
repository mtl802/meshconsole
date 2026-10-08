// worker.go 为 agent 侧命令执行 worker（SPEC-M1d §2：独立执行 worker，
// **禁止阻塞心跳循环**——runner.go 旧形态是采集→HTTP→休眠且丢弃响应体，
// M1d 起心跳响应中的命令交付本 worker 异步执行）。
//
// 结构：单队列 + 固定 2 个执行 goroutine（SPEC §3：agent 本地并发执行 ≤2）；
// 交付（Deliver）只入队不执行，心跳循环永不为命令等待。
//
// 双重校验的 agent 端（SPEC §3）：拒绝未知 kind；unit 槽须命中自身配置声明的
// 受管 systemd 服务集合（console 侧按其服务清单校验，两端各自完整校验）；
// l2_enabled=false 的节点拒绝一切执行（总开关，安全缺省）——拒绝以 failed
// 回执如实上报（面板可见原因），绝不静默丢弃。
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/mtl802/meshconsole/internal/cmdkind"
	"github.com/mtl802/meshconsole/internal/config"
)

// workerConcurrency 为本地并发执行上限（SPEC-M1d §3：≤2）。
const workerConcurrency = 2

// maxExecTimeoutS 为执行超时上限（console 侧提交校验同口径，双端不信任）。
const maxExecTimeoutS = 300

// CommandWorker 为命令执行 worker。
//
// 租约代次对齐（R39-#2，R41-B1 深修为显式代次令牌）：以 console 下发的
// lease_epoch（首次领取=1，租约过期重领自增）作为**代次令牌**——worker 只为其
// 持有代次执行、上报与对账；领取响应若发现同 id 命令的代次已前移（已被重领），
// 旧代次静默退役（终止在途执行、定点删除旧代次对账文件、结果永不落盘上报），
// 新代次作为全新命令执行。本地对账文件按 (command_id, epoch) 命名——旧代次的
// drop/ACK 清理只触达该代次及更早的文件，结构上不可能误删新代次刚写入的结果；
// console 侧回执校验同样按 epoch 拒绝旧代次迟到回执——双端同一令牌，执行超
// 租约后不双重执行、终态可对账。
type CommandWorker struct {
	cfg   *config.Agent
	store *CommandStore
	log   *slog.Logger
	cwd   string
	caps  []string
	queue chan *CommandRecord
	mu    sync.Mutex
	// started 为 command_id → 当前持有的租约代次（console 下发的 lease_epoch；
	// 同代次重发去重，代次前移触发退役）。
	started map[string]int64
	// cancels 为在途执行的取消函数（代次前移时即时终止旧进程组，避免新旧
	// 两代同时执行）。
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// delivery 为心跳响应下发的一条命令（registry commandOut 的 agent 侧形态）。
type delivery struct {
	CommandID  string `json:"command_id"`
	Kind       string `json:"kind"`
	ArgsJSON   string `json:"args_json"`
	TimeoutS   int64  `json:"timeout_s"`
	ClaimedAt  int64  `json:"claimed_at"`
	LeaseEpoch int64  `json:"lease_epoch"`
}

// ackEntry / dropEntry 为心跳响应 ack_ids / drop_ids 条目（registry 同名形态：
// 条目携带租约代次，清理只触达该代次及更早的本地对账文件）。
type ackEntry struct {
	CommandID  string `json:"command_id"`
	LeaseEpoch int64  `json:"lease_epoch"`
}

type dropEntry struct {
	CommandID  string `json:"command_id"`
	LeaseEpoch int64  `json:"lease_epoch"`
}

// NewCommandWorker 构造 worker（含能力自检与中断恢复），并启动执行 goroutine。
// state.StateFile 为 state 文件路径（对账目录 commands/ 取其同目录，SPEC：
// 结果目录 state 同目录）；StateFile 为空视为致命配置（对账无处落盘）。
func NewCommandWorker(ctx context.Context, cfg *config.Agent, st *State, log *slog.Logger) (*CommandWorker, error) {
	if st.StateFile == "" {
		return nil, fmt.Errorf("state file path missing: command store unavailable")
	}
	cs, err := NewCommandStore(st.StateFile)
	if err != nil {
		return nil, err
	}
	if removed, err := cs.PurgeLegacy(); err != nil {
		log.Error("purge legacy command records", "err", err)
	} else if len(removed) > 0 {
		// 升级残留（无代次旧版对账文件）：无法参与代次对账，清除后由 console
		// 租约过期重发兜底，不丢账。
		log.Warn("legacy command records purged (pre-epoch upgrade residue)", "count", len(removed))
	}
	w := &CommandWorker{
		cfg:     cfg,
		store:   cs,
		log:     log,
		cwd:     st.StateFileDir(),
		caps:    detectCaps(cfg),
		queue:   make(chan *CommandRecord, 32),
		started: map[string]int64{},
		cancels: map[string]context.CancelFunc{},
	}
	w.restoreInterrupted()
	for i := 0; i < workerConcurrency; i++ {
		w.wg.Add(1)
		go w.loop(ctx)
	}
	log.Info("command worker started", "concurrency", workerConcurrency,
		"l2_enabled", cfg.L2Enabled, "caps", w.caps)
	return w, nil
}

// detectCaps 能力自检（SPEC-M1d §5 平台协商）：按平台探测 kind 依赖的可执行
// 路径，诚实声明——不上报未验证的能力。windows 预留 windows-*（v1 无 kind）。
func detectCaps(cfg *config.Agent) []string {
	var caps []string
	switch runtime.GOOS {
	case "linux":
		if fileExists("/usr/bin/systemctl") {
			caps = append(caps, "linux-systemd")
		}
		if p, err := exec.LookPath(cfg.DockerBin); err == nil && p != "" {
			caps = append(caps, "linux-docker")
		}
	case "darwin":
		if fileExists("/bin/launchctl") {
			caps = append(caps, "mac-launchd")
		}
	case "windows":
		caps = append(caps, "windows") // 预留，v1 无 windows-* kind
	}
	return caps
}

// Caps 返回本机能力清单（心跳 caps 字段；l2_enabled=false 时为空——console
// 提交侧 caps 校验即拒绝，从源头阻止下发）。
func (w *CommandWorker) Caps() []string {
	if !w.cfg.L2Enabled {
		return nil
	}
	return w.caps
}

// restoreInterrupted 崩溃恢复对账（DESIGN §4.1-D）：intent 已落盘但无结果 →
// 补写 interrupted 结果（不重执行），随心跳上报；console 置 unknown 对齐。
// result 俱全的照旧上报直至 ACK。
func (w *CommandWorker) restoreInterrupted() {
	recs, bad, err := w.store.LoadAll()
	if err != nil {
		w.log.Error("load command records", "err", err)
		return
	}
	for _, name := range bad {
		w.log.Warn("corrupt command record skipped", "file", name)
	}
	for _, rec := range recs {
		if rec.Phase == PhaseResult {
			w.markStarted(rec.CommandID, rec.LeaseEpoch) // 已有结果：不再执行，仅待 ACK
			continue
		}
		w.markStarted(rec.CommandID, rec.LeaseEpoch)
		res := &CommandRecord{
			CommandID: rec.CommandID, LeaseEpoch: rec.LeaseEpoch,
			Kind: rec.Kind, ArgsJSON: rec.ArgsJSON,
			TimeoutS: rec.TimeoutS, ClaimedAt: rec.ClaimedAt,
			Interrupted: true, FinishedAt: time.Now().Unix(),
			ResultText: "agent 重启：执行中断（intent 已落盘但无结果），按协议不重执行，置 unknown 对齐",
		}
		if err := w.store.SaveResult(res); err != nil {
			w.log.Error("save interrupted result", "command_id", rec.CommandID, "err", err)
		}
		w.log.Warn("command interrupted by restart", "command_id", rec.CommandID, "kind", rec.Kind)
	}
}

// Deliver 交付心跳响应中的一批命令（非阻塞：队列满说明本地积压异常，拒绝并
// 记日志——console 租约过期会重发）。按代次去重（R41-B1）：同 command_id 且
// 同 lease_epoch 的重发跳过（结果已在本地或执行中，回执持续重发直至 ACK）；
// 代次前移（租约过期被重领）则旧代次静默退役、新代次全新执行；代次倒退的
// 交付（乱序/过期响应）防御性忽略。
func (w *CommandWorker) Deliver(cmds []delivery) {
	for _, d := range cmds {
		if d.CommandID == "" || d.Kind == "" {
			continue
		}
		if d.LeaseEpoch < 1 {
			w.log.Warn("command delivery without lease epoch, ignored",
				"command_id", d.CommandID)
			continue
		}
		if prev, ok := w.generation(d.CommandID); ok {
			if prev == d.LeaseEpoch {
				continue
			}
			if prev > d.LeaseEpoch {
				w.log.Warn("stale command delivery ignored: held generation is newer",
					"command_id", d.CommandID, "held", prev, "delivered", d.LeaseEpoch)
				continue
			}
			w.retire(d.CommandID, d.LeaseEpoch)
		} else {
			w.markStarted(d.CommandID, d.LeaseEpoch)
		}
		rec := &CommandRecord{
			CommandID: d.CommandID, LeaseEpoch: d.LeaseEpoch, Kind: d.Kind,
			ArgsJSON: d.ArgsJSON, TimeoutS: d.TimeoutS, ClaimedAt: d.ClaimedAt,
		}
		if err := w.store.SaveIntent(rec); err != nil {
			w.log.Error("save command intent", "command_id", d.CommandID, "err", err)
			// intent 落盘失败不执行（执行而不可对账比不执行更危险）：
			// 直接落失败回执，console 侧可见。
			w.recordRefusal(rec, "本地对账落盘失败，拒绝执行")
			continue
		}
		select {
		case w.queue <- rec:
		default:
			w.log.Warn("command queue full, refusing", "command_id", d.CommandID)
			w.recordRefusal(rec, "本地执行队列已满，拒绝执行")
		}
	}
}

// retire 把命令推进到新租约代次并静默退役旧代次：代次标记先行换入（在途执行
// 收尾的代次复核必然失配，结果不再落盘/上报），随后即时终止旧执行（进程组
// kill，避免新旧两代同时跑），并定点删除旧代次的对账文件（只触达旧代次——
// R41-B1：文件按代次命名，新代次的 intent/result 不可能被误删）。不重执行
// 旧代次、不误报。
func (w *CommandWorker) retire(id string, gen int64) {
	w.mu.Lock()
	oldGen := w.started[id]
	w.started[id] = gen
	cancel := w.cancels[id]
	delete(w.cancels, id)
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if oldGen > 0 && oldGen != gen {
		if err := w.store.Delete(id, oldGen); err != nil {
			w.log.Error("delete retired command record", "command_id", id, "err", err)
		}
	}
	w.log.Warn("command superseded by new lease generation, old execution retired",
		"command_id", id, "generation", gen)
}

// loop 为执行 goroutine：拒绝性校验（未知 kind / l2 总开关 / 受管集合）在
// worker 内完成，白名单 kind 经 cmdkind 再校验后 exec。
func (w *CommandWorker) loop(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-w.queue:
			w.run(ctx, rec)
		}
	}
}

// managedUnit 报告 unit 是否在自身配置声明的受管 systemd 服务集合内。
func (w *CommandWorker) managedUnit(unit string) bool {
	for _, s := range w.cfg.Services {
		if s.Type == "systemd" && s.Target == unit {
			return true
		}
	}
	return false
}

// recordRefusal 以 failed 回执如实记录拒绝原因（exit_code 缺失 = 未执行）。
// 落盘经代次复核：拒绝也只为其持有代次记录（拒绝路径与执行路径同口径）。
func (w *CommandWorker) recordRefusal(rec *CommandRecord, reason string) {
	res := &CommandRecord{
		CommandID: rec.CommandID, LeaseEpoch: rec.LeaseEpoch,
		Kind: rec.Kind, ArgsJSON: rec.ArgsJSON,
		TimeoutS: rec.TimeoutS, ClaimedAt: rec.ClaimedAt,
		ResultText: reason, FinishedAt: time.Now().Unix(),
	}
	w.saveResultIfCurrent(res)
}

// saveResultIfCurrent 在「代次复核 + 结果落盘」同一临界区内完成（R39-#2）：
// 租约过期被重领（代次前移）的旧执行在此静默退役——不落盘、不上报，其结果
// 永不进入心跳。临界区闭合 retire 换入新代次与旧执行落盘之间的竞态窗口。
func (w *CommandWorker) saveResultIfCurrent(rec *CommandRecord) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started[rec.CommandID] != rec.LeaseEpoch {
		w.log.Warn("command result discarded: lease generation superseded",
			"command_id", rec.CommandID)
		return false
	}
	if err := w.store.SaveResult(rec); err != nil {
		w.log.Error("save command result", "command_id", rec.CommandID, "err", err)
	}
	return true
}

func (w *CommandWorker) run(ctx context.Context, rec *CommandRecord) {
	// 入队后的代次复核（快速路径）：排队期间已被重领（代次前移）→ 静默退役，
	// 不做后续校验。权威复核在下方 cancel 注册临界区（两段缺一不可：快速路径
	// 省去死代次的校验开销，临界区保证 retire 与 run 任何交错只保留一代持有
	// 取消柄——否则旧代次注册的取消柄 retire 读不到，新旧两代并行执行）。
	if gen, ok := w.generation(rec.CommandID); !ok || gen != rec.LeaseEpoch {
		w.log.Warn("command retired while queued: lease generation superseded",
			"command_id", rec.CommandID)
		return
	}
	// agent 端独立校验（SPEC-M1d §3：console 与 agent 双重校验；拒绝不执行）。
	if !w.cfg.L2Enabled {
		w.recordRefusal(rec, "本节点未启用任务下发（agent 配置 l2_enabled: false），拒绝执行")
		return
	}
	if cmdkind.Lookup(rec.Kind) == nil {
		w.recordRefusal(rec, fmt.Sprintf("未知命令类型 %q，拒绝执行（agent 端白名单校验）", rec.Kind))
		return
	}
	if rec.TimeoutS < 1 || rec.TimeoutS > maxExecTimeoutS {
		w.recordRefusal(rec, fmt.Sprintf("timeout_s=%d 超出值域 1-%d，拒绝执行", rec.TimeoutS, maxExecTimeoutS))
		return
	}
	argsJSON, err := cmdkind.ValidateArgs(rec.Kind, rec.ArgsJSON, w.managedUnit)
	if err != nil {
		w.recordRefusal(rec, fmt.Sprintf("参数校验失败（agent 端白名单校验）：%v", err))
		return
	}
	argv, err := cmdkind.BuildArgv(rec.Kind, argsJSON, w.cfg.DockerBin)
	if err != nil {
		w.recordRefusal(rec, fmt.Sprintf("argv 构建失败：%v", err))
		return
	}
	// 执行挂独立可取消 ctx：「代次复核 + 取消柄注册」同一临界区收口（R39-#2）。
	// 校验期间被重领 → 此处失配即静默退役（不执行、不注册）；retire 在临界区
	// 之前发生 → 复核失配同样拦截，不存在「retire 读不到取消柄」的窗口。
	xctx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	if gen, ok := w.started[rec.CommandID]; !ok || gen != rec.LeaseEpoch {
		w.mu.Unlock()
		cancel()
		w.log.Warn("command retired before execution: lease generation superseded",
			"command_id", rec.CommandID)
		return
	}
	w.cancels[rec.CommandID] = cancel
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		// 仅当本代仍持有执行权才摘除取消柄：代次已前移时新代注册的柄不可被
		// 旧代次的收尾误删（误删 = 新代次失去被 retire 终止的能力）。
		if w.started[rec.CommandID] == rec.LeaseEpoch {
			delete(w.cancels, rec.CommandID)
		}
		w.mu.Unlock()
		cancel()
	}()
	w.log.Info("command executing", "command_id", rec.CommandID, "kind", rec.Kind)
	exitCode, text, execErr := Execute(xctx, argv, rec.TimeoutS, w.cwd)
	if execErr != nil && !isTimeoutErr(execErr) {
		// 启动失败（二进制缺失等）：exit_code 缺失 + 原因说明。说明走
		// finalizeResult 预算口径追加（R39-#3：text 可能已满 64KB，直接字符串
		// 拼接会突破回执硬上限、被 console 整条拒收）。
		text = finalizeResult(text, false, "\n[meshagent] 启动失败："+execErr.Error())
	}
	res := &CommandRecord{
		CommandID: rec.CommandID, LeaseEpoch: rec.LeaseEpoch,
		Kind: rec.Kind, ArgsJSON: argsJSON,
		TimeoutS: rec.TimeoutS, ClaimedAt: rec.ClaimedAt,
		ExitCode: exitCode, ResultText: text, FinishedAt: time.Now().Unix(),
	}
	// 回执只为其持有代次落盘上报（R41-B1）：执行超租约被重领的旧代次在此
	// 静默退役——console 侧新代次回执可对账，不出现双重执行。
	w.saveResultIfCurrent(res)
	w.log.Info("command executed", "command_id", rec.CommandID, "kind", rec.Kind,
		"exit_code", exitCodeVal(exitCode), "interrupted", false)
}

func isTimeoutErr(err error) bool { return err == errExecTimeout }

func exitCodeVal(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Ack 删除已确认命令的本地对账文件（console ack_ids 到达才删——防丢回执）。
// 条目携带已记录回执的租约代次（R41-B1）：只删该代次及更早的对账文件
// （DeleteUpTo），更高代次的在途执行与结果不可触达。代次守卫：仅当当前持有
// 代次 ≤ 被 ACK 代次才摘除在途标记（正常协议下 ack 隐含终态、终态不可重领，
// 两者不可能错位；守卫保证即便出现异常序列也不会抹掉更高代次的执行权）。
func (w *CommandWorker) Ack(entries []ackEntry) {
	for _, e := range entries {
		if err := w.store.DeleteUpTo(e.CommandID, e.LeaseEpoch); err != nil {
			w.log.Error("delete acked command record", "command_id", e.CommandID, "err", err)
			continue
		}
		if gen, ok := w.generation(e.CommandID); !ok || gen <= e.LeaseEpoch {
			w.unmarkStarted(e.CommandID)
		}
		w.log.Info("command result acked", "command_id", e.CommandID, "lease_epoch", e.LeaseEpoch)
	}
}

// Drop 删除无归属/被拒回执的本地副本（console 明确告知不收，重发无意义）。
// 条目携带被拒回执的租约代次（R41-B1 收口）：定点删除 (command_id, epoch)
// 一个文件——旧代次回执的 drop 结构上触达不了新代次刚写入的结果文件（原
// 「一命令一文件」形态下同拍「Deliver 新代次 + Drop 旧代次」会误删新结果，
// 回执丢失后 console 判 unknown 重执行）。intent 文件属于在途执行，不删；
// 在途标记仅当被删回执与当前持有代次同代才摘除——新代次的执行权不受
// 旧代次 drop 影响。
func (w *CommandWorker) Drop(entries []dropEntry) {
	for _, e := range entries {
		rec, err := w.store.Load(e.CommandID, e.LeaseEpoch)
		if err != nil {
			w.log.Error("load dropped command record", "command_id", e.CommandID, "err", err)
			continue
		}
		if rec == nil {
			continue
		}
		if rec.Phase != PhaseResult {
			w.log.Warn("command drop ignored: record is an in-flight intent",
				"command_id", e.CommandID, "lease_epoch", e.LeaseEpoch)
			continue
		}
		if err := w.store.Delete(e.CommandID, e.LeaseEpoch); err != nil {
			w.log.Error("delete dropped command record", "command_id", e.CommandID, "err", err)
			continue
		}
		if gen, ok := w.generation(e.CommandID); ok && gen == e.LeaseEpoch {
			w.unmarkStarted(e.CommandID)
		}
		w.log.Warn("command result dropped by console",
			"command_id", e.CommandID, "lease_epoch", e.LeaseEpoch)
	}
}

// ResultsSnapshot 返回待上报的结果清单（未 ACK 持续重发）。
func (w *CommandWorker) ResultsSnapshot() []*CommandRecord {
	recs, _, err := w.store.LoadAll()
	if err != nil {
		w.log.Error("list command records", "err", err)
		return nil
	}
	out := make([]*CommandRecord, 0, len(recs))
	for _, r := range recs {
		if r.Phase == PhaseResult {
			out = append(out, r)
		}
	}
	return out
}

// RunningIDs 返回本地正在执行（intent 已落盘、结果未出）的命令 id。
func (w *CommandWorker) RunningIDs() []string {
	recs, _, err := w.store.LoadAll()
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range recs {
		if r.Phase == PhaseIntent {
			out = append(out, r.CommandID)
		}
	}
	return out
}

// generation 返回命令当前持有的租约代次（lease_epoch）；未持有返回 ok=false。
func (w *CommandWorker) generation(id string) (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	gen, ok := w.started[id]
	return gen, ok
}

func (w *CommandWorker) markStarted(id string, gen int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started[id] = gen
}

func (w *CommandWorker) unmarkStarted(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.started, id)
	delete(w.cancels, id)
}

// fileExists 探测可执行路径存在性：绝对路径直接 stat（受信目录），相对名走
// PATH 解析（docker_bin 等配置可能就是 "docker"）。
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) {
		_, err := os.Stat(p)
		return err == nil
	}
	_, err := exec.LookPath(p)
	return err == nil
}
