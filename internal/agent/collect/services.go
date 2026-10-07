// 服务状态查询（DESIGN §4.3、SPEC-M1b-a §2）。
//
// 安全口径：一律 exec 固定 argv（无 shell），target 已在配置加载时按类型白名单
// 校验（systemd/docker 字符集 [a-zA-Z0-9_@.-] 且不以 - 开头；process 禁控制字符
// 与开头 -）。docker 只走只读 CLI 查询，不经 SDK 直连 socket（DESIGN §7-5）；
// 二进制路径可配置（docker_bin，默认 "docker"，生产建议指向只读 helper/
// socket-proxy 包装，R10-#4）。
//
// 状态语义（禁止编造，查询不到就如实报）：
//   - active / inactive / failed：查询本身成功得出的结论；
//   - unavailable：执行环境不可用（二进制缺失、docker 无 daemon、权限不足）；
//   - unknown：查询执行了但结果无法判定（超时、不可解读的输出/退出码）。
package collect

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
)

// queryTimeout 单条服务状态查询的超时（exec 全部绑 ctx，防卡死心跳循环）。
const queryTimeout = 5 * time.Second

// scanBudget 为整轮服务查询的总预算（R11-G）：32 条 × 5s 的病态最坏情况会阻塞
// 心跳超过 offline_after（60s），整轮封顶 10s——超预算未执行的条目如实报
// unknown + 说明（不编造），下一轮心跳重查。
const scanBudget = 10 * time.Second

// maxExecOutput 为单条命令 stdout/stderr 各自的收集上限（R10-#5，DESIGN §7-9
// 资源限额）：在写入阶段封顶，异常命令的海量输出不进 agent 内存；触顶截断并在
// 该条结果 detail 标注 truncated。
const maxExecOutput = 64 << 10

// truncNote 为截断标注文本（detail 中如实说明）。
const truncNote = "output truncated at 64KB"

// ServiceStatus 为一条服务状态查询结果（JSON 对应 console services 表行）。
type ServiceStatus struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Target string `json:"target"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// cmdResult 汇总一次外部命令执行的输出；truncated 表示 stdout 或 stderr 触及
// 64KB 上限被截断（调用方须在结果 detail 中如实标注）。
type cmdResult struct {
	stdout, stderr string
	truncated      bool
}

// cmdRunner 抽象外部命令执行（单测注入 canned 输出，不打真机）。
type cmdRunner interface {
	Run(ctx context.Context, name string, args ...string) (cmdResult, error)
}

// cappedBuffer 写入封顶的缓冲：超过 max 后丢弃后续字节并置位 truncated；
// Write 始终报告全量写入，令 io.Copy/Write 语义不中断（丢弃即截断语义）。
type cappedBuffer struct {
	max       int
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

type execRunner struct{}

// Run 以固定 argv 执行命令；stdout/stderr 经 cappedBuffer 收集（各 64KB 封顶，
// R10-#5）。WaitDelay 防 CommandContext 只杀直接子进程时，孙进程持有输出管道
// 令 Run() 悬挂（服务查询宁可截断输出也不卡心跳循环）。
func (execRunner) Run(ctx context.Context, name string, args ...string) (cmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, errOut := &cappedBuffer{max: maxExecOutput}, &cappedBuffer{max: maxExecOutput}
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.WaitDelay = 500 * time.Millisecond
	err := cmd.Run()
	return cmdResult{
		stdout:    out.buf.String(),
		stderr:    errOut.buf.String(),
		truncated: out.truncated || errOut.truncated,
	}, err
}

// ServiceChecker 按配置声明周期性查询受管服务状态。
type ServiceChecker struct {
	decls     []config.ServiceDecl
	dockerBin string
	runner    cmdRunner
	// budget 为整轮查询总预算（测试可注入）；零值取 scanBudget。
	budget time.Duration
}

// NewServiceChecker 构造；decls 已经过 LoadAgent 校验（type/target 合法）。
// dockerBin 为 docker CLI 路径（配置 agent.docker_bin；空值取 "docker"）。
func NewServiceChecker(decls []config.ServiceDecl, dockerBin string) *ServiceChecker {
	if dockerBin = strings.TrimSpace(dockerBin); dockerBin == "" {
		dockerBin = "docker"
	}
	return &ServiceChecker{decls: decls, dockerBin: dockerBin, runner: execRunner{}}
}

// CheckAll 逐条查询（串行：M1 规模 ≤32 条、单条 5s 超时，正常环境远低于心跳
// 周期）。整轮受总预算约束（R11-G，10s）：预算耗尽后剩余条目不再执行，如实报
// unknown + 说明。任一条失败只影响该条，不影响其他条。
func (c *ServiceChecker) CheckAll(ctx context.Context) []ServiceStatus {
	if len(c.decls) == 0 {
		return nil
	}
	budget := c.budget
	if budget <= 0 {
		budget = scanBudget
	}
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	out := make([]ServiceStatus, 0, len(c.decls))
	for _, d := range c.decls {
		if bctx.Err() != nil {
			st := ServiceStatus{Name: d.Name, Type: d.Type, Target: d.Target, Status: "unknown"}
			st.Detail = truncErr("service scan budget (" + budget.String() + ") exhausted before this entry")
			out = append(out, st)
			continue
		}
		out = append(out, c.checkOne(bctx, d))
	}
	return out
}

func (c *ServiceChecker) checkOne(ctx context.Context, d config.ServiceDecl) ServiceStatus {
	st := ServiceStatus{Name: d.Name, Type: d.Type, Target: d.Target, Status: "unknown"}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	switch d.Type {
	case "systemd":
		c.checkSystemd(qctx, &st)
	case "docker":
		c.checkDocker(qctx, &st)
	case "process":
		c.checkProcess(qctx, &st)
	default:
		// LoadAgent 已拒绝非法 type；此处兜底如实报 unknown。
		st.Detail = "unsupported service type"
	}
	return st
}

// checkSystemd：`systemctl is-active <unit>`。systemd 同时以退出码与 stdout 文本
// 表达状态（R10-#3，man systemctl is-active）：0=active（含 activating 等进行
// 态）、3=inactive/dead（部分版本 failed 也落此码）、其他非零=unit 不存在/DBus
// 错误等。口径：先按退出码分类，文本精确匹配可精化（failed/inactive），都不足
// 以判定时报 unknown+原文；只有执行器自身失败（二进制缺失等非 ExitError）才是
// unavailable。
func (c *ServiceChecker) checkSystemd(ctx context.Context, st *ServiceStatus) {
	res, err := c.runner.Run(ctx, "systemctl", "is-active", st.Target)
	defer markTruncated(st, res)
	out := strings.TrimSpace(res.stdout)
	if err != nil {
		if ctx.Err() != nil {
			st.Status = "unknown"
			st.Detail = "systemctl query timeout"
			return
		}
		var ec exitCoder
		if !errors.As(err, &ec) {
			// 执行器错误（systemctl 缺失于 macOS/Windows、无法 fork 等）：
			// 环境不可用，如实说明。
			st.Status = "unavailable"
			st.Detail = truncErr("systemctl: " + err.Error())
			return
		}
		switch ec.ExitCode() {
		case 3:
			// 非活跃族：inactive/dead（failed 文本可精化；activating 等不可
			// 判定的文本不编造）。
			switch out {
			case "failed":
				st.Status = "failed"
			case "", "inactive":
				st.Status = "inactive"
			default:
				st.Status = "unknown"
				st.Detail = truncErr("systemctl is-active: " + out)
			}
		default:
			// 退出码 0 出错（不应发生）与其他非零（unit 不存在、DBus 错误等）：
			// 以文本映射，无文本/不可解读不编造。
			systemdTextStatus(out, st)
		}
		return
	}
	// 退出码 0：active——文本仍须核对，异常文本说明输出不可解读，不编造。
	systemdTextStatus(out, st)
}

// systemdTextStatus 按 is-active 的 stdout 文本映射：active/inactive/failed 直映；
// 空输出与中间态（activating/reloading/unknown/maintenance…）一律 unknown+原文。
func systemdTextStatus(out string, st *ServiceStatus) {
	switch out {
	case "active":
		st.Status = "active"
	case "inactive":
		st.Status = "inactive"
	case "failed":
		st.Status = "failed"
	case "":
		st.Status = "unknown"
		st.Detail = "systemctl is-active returned no output"
	default:
		st.Status = "unknown"
		st.Detail = truncErr("systemctl is-active: " + out)
	}
}

// markTruncated 输出被截断时在该条 detail 如实标注（R10-#5）。
func markTruncated(st *ServiceStatus, res cmdResult) {
	if !res.truncated {
		return
	}
	if st.Detail == "" {
		st.Detail = truncNote
		return
	}
	st.Detail = truncErr(st.Detail + "; " + truncNote)
}

// checkDocker：`<docker_bin> ps --filter name=<target> --format '{{.State}}'`
// （只读 CLI）。二进制缺失或 daemon 不可达 → unavailable + 错误说明（SPEC-M1b-a §2）。
func (c *ServiceChecker) checkDocker(ctx context.Context, st *ServiceStatus) {
	res, err := c.runner.Run(ctx, c.dockerBin, "ps", "--filter", "name="+st.Target, "--format", "{{.State}}")
	defer markTruncated(st, res)
	out := res.stdout
	if err != nil {
		if ctx.Err() != nil {
			st.Status = "unknown"
			st.Detail = "docker query timeout"
			return
		}
		st.Status = "unavailable"
		msg := strings.TrimSpace(res.stderr)
		if msg == "" {
			msg = err.Error()
		}
		st.Detail = truncErr("docker: " + msg)
		return
	}
	lines := nonEmptyLines(out)
	switch {
	case len(lines) == 0:
		// docker ps 只列运行中的容器：无匹配行 = 目标容器未在运行。
		st.Status = "inactive"
		st.Detail = "no running container matches name filter"
	case len(lines) == 1 && lines[0] == "running":
		st.Status = "active"
	default:
		st.Status = "unknown"
		st.Detail = truncErr("docker ps state: " + strings.Join(lines, ","))
	}
}

// checkProcess：`pgrep -f <target>` 计数（macOS 兜底；pgrep 缺失的裸 Windows
// 会走 unavailable + 说明，不编造状态——Windows 真机表现列 M1b-b 上机项）。
func (c *ServiceChecker) checkProcess(ctx context.Context, st *ServiceStatus) {
	res, err := c.runner.Run(ctx, "pgrep", "-f", st.Target)
	defer markTruncated(st, res)
	out := res.stdout
	if err != nil {
		if ctx.Err() != nil {
			st.Status = "unknown"
			st.Detail = "pgrep query timeout"
			return
		}
		// 按退出码区分：pgrep 约定 exit 1 = 无匹配、exit ≥2 = 语法/系统错误；
		// 非 ExitError（二进制缺失等 exec 层失败）= 环境不可用。
		var ec exitCoder
		switch {
		case errors.As(err, &ec) && ec.ExitCode() == 1:
			st.Status = "inactive"
		case errors.As(err, &ec) && ec.ExitCode() >= 2:
			st.Status = "unknown"
			st.Detail = truncErr("pgrep: " + err.Error())
		default:
			st.Status = "unavailable"
			st.Detail = truncErr("pgrep: " + err.Error())
		}
		return
	}
	n := len(nonEmptyLines(out))
	if n == 0 {
		// exit 0 却无输出行：不可判定，不当作 active。
		st.Status = "unknown"
		st.Detail = "pgrep exited 0 with no output"
		return
	}
	st.Status = "active"
	st.Detail = "count=" + strconv.Itoa(n)
}

// exitCoder 与 os/exec 的 ExitError 对齐（ExitError.ExitCode）；
// 以接口判断便于测试注入与未来替换执行途径。
type exitCoder interface {
	ExitCode() int
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}
