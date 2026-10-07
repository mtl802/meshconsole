// Package agentdisc 实现 AI agent 发现（DESIGN §4.2-A，SPEC-M1b-a §3）。
//
// 双轨机制：
//  1. 内置已知清单（默认 zcode/codex/claude/gemini/aider，可配置增删）：
//     PATH 存在性（exec.LookPath）+ `--version`（5s 超时）取版本；
//     不在 PATH 上的条目不上报（一个不存在的 CLI 不出现）。
//  2. 配置显式声明：custom（CLI 型，直接执行声明路径）与 services（服务型，
//     仅本地端口探测存活性——不可 invoke，DESIGN §1 v1 边界）。
//
// 合并规则：同名时显式声明覆盖 PATH 探测结果（声明优先）。
// 扫描频率低频（默认 5 分钟），结果由调用方缓存并随心跳上报。
// 安全边界：发现登记 ≠ 可调用——上报的 path 仅作参考信息，
// invokable 恒 false 由 console 端强制（DESIGN §4.2-B）。
package agentdisc

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mtl802/meshconsole/internal/config"
)

const (
	// VersionTimeout 为 --version 探测超时（SPEC-M1b-a §3：5s）。
	VersionTimeout = 5 * time.Second
	// PortProbeTimeout 为服务型 agent 端口探测超时。
	PortProbeTimeout = 2 * time.Second
	// maxExecOutput 为版本探测 stdout/stderr 各自的收集上限（R10-#5，DESIGN
	// §7-9：写入阶段封顶，异常命令的海量输出不进 agent 内存）。
	maxExecOutput = 64 << 10
	maxVersionLen = 128
	maxDetailLen  = 200
	maxPathLen    = 512
	truncNote     = "output truncated at 64KB"
)

// Report 为一条 agent 发现结果（JSON 随心跳上报，字段与 console 校验对齐）。
type Report struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // cli | service
	Version string `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
	Status  string `json:"status"` // active | inactive | unavailable
	Detail  string `json:"detail,omitempty"`
}

// Scanner 执行一轮发现扫描。字段在 New 时固定（配置不热加载）。
type Scanner struct {
	known    []string
	custom   []config.CustomAgent
	services []config.CustomAgentService

	// 超时可注入（测试）；零值取默认。
	VersionTimeout time.Duration
	PortTimeout    time.Duration
}

// New 构造扫描器。known 传 nil 时取内置默认清单。
func New(known []string, custom []config.CustomAgent, services []config.CustomAgentService) *Scanner {
	if known == nil {
		known = config.DefaultKnownAgents()
	}
	return &Scanner{
		known:    known,
		custom:   custom,
		services: services,
	}
}

// Scan 执行一轮扫描并返回合并结果（按名称排序，保证上报稳定）。
// ctx 取消时中断进行中的探测（未完成的条目按 unavailable 如实上报，不编造状态）。
func (s *Scanner) Scan(ctx context.Context) []Report {
	results := map[string]Report{}
	// 顺序：先 PATH 探测，后被声明覆盖（同名声明优先）。
	for _, name := range s.known {
		if r, found := s.lookPathProbe(ctx, name); found {
			results[r.Name] = r
		}
	}
	for _, c := range s.custom {
		results[c.Name] = s.customCLIProbe(ctx, c)
	}
	for _, svc := range s.services {
		results[svc.Name] = s.portProbe(ctx, svc)
	}

	out := make([]Report, 0, len(results))
	for _, r := range results {
		out = append(out, r)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// versionTimeout / portTimeout：显式覆盖优先，否则取默认。
func (s *Scanner) versionTimeout() time.Duration {
	if s.VersionTimeout > 0 {
		return s.VersionTimeout
	}
	return VersionTimeout
}

func (s *Scanner) portTimeout() time.Duration {
	if s.PortTimeout > 0 {
		return s.PortTimeout
	}
	return PortProbeTimeout
}

// lookPathProbe 探测 PATH 上的已知 CLI；不存在时 found=false（不上报）。
func (s *Scanner) lookPathProbe(ctx context.Context, name string) (Report, bool) {
	r := Report{Name: name, Type: "cli"}
	path, err := exec.LookPath(name)
	if err != nil {
		return Report{}, false
	}
	r.Path = trunc(path, maxPathLen)
	version, detail := s.runVersion(ctx, path, "--version")
	if detail != "" {
		// 在 PATH 上但版本探测失败（超时/无执行权限）：如实报 unavailable，
		// path 仅是发现路径、不代表可调用（SPEC-M1b-a §3）。
		r.Status = "unavailable"
		r.Detail = detail
		return r, true
	}
	r.Status = "active"
	r.Version = version
	return r, true
}

// customCLIProbe 探测显式声明的 CLI（command 已在配置加载时强制绝对路径）。
func (s *Scanner) customCLIProbe(ctx context.Context, c config.CustomAgent) Report {
	r := Report{Name: c.Name, Type: "cli", Path: trunc(c.Command, maxPathLen)}
	version, detail := s.runVersion(ctx, c.Command, c.VersionFlag)
	if detail != "" {
		r.Status = "unavailable"
		r.Detail = detail
		return r
	}
	r.Status = "active"
	r.Version = version
	return r
}

// portProbe 服务型 agent：仅本地 TCP 端口探测存活性（DESIGN §4.2-A：
// 服务型 agent v1 只发现登记，不做任何协议交互/调用）。DialContext 绑定
// ctx（R10-#6）：扫描被取消/超时时探测立即中断，按 unavailable 如实上报。
func (s *Scanner) portProbe(ctx context.Context, svc config.CustomAgentService) Report {
	r := Report{Name: svc.Name, Type: "service"}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(svc.Port))
	d := net.Dialer{Timeout: s.portTimeout()}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			// 探测被打断：结论未知，不编造 inactive。
			r.Status = "unavailable"
			r.Detail = trunc("tcp probe "+addr+" cancelled: "+ctx.Err().Error(), maxDetailLen)
			return r
		}
		r.Status = "inactive"
		r.Detail = trunc("tcp probe "+addr+": "+err.Error(), maxDetailLen)
		return r
	}
	conn.Close()
	r.Status = "active"
	return r
}

// cappedBuffer 写入封顶的缓冲（与 collect 包同口径，R10-#5）：超过 max 后丢弃
// 后续字节并置位 truncated；Write 始终报告全量写入，不中断 io.Copy/Write。
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

// String 返回已捕获（可能被截断）的内容。
func (b *cappedBuffer) String() string { return b.buf.String() }

// runVersion 执行 `<command> <versionFlag>` 并返回（首行版本, 失败说明）。
// 失败说明非空时版本不可信（超时/退出非零/无输出），调用方据此报 unavailable。
// stdout/stderr 经 cappedBuffer 各 64KB 封顶（R10-#5）；版本值取自哪个缓冲，
// 就按该缓冲的截断状态判断选中行的完整性（R13-#4）：选中行有换行终止才完整
// 可信，无换行终止（含 stdout 空行+截断、stderr 截断）不得冒充版本。
func (s *Scanner) runVersion(ctx context.Context, command, versionFlag string) (version, detail string) {
	timeout := s.versionTimeout()
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(vctx, command, versionFlag)
	out, errBuf := &cappedBuffer{max: maxExecOutput}, &cappedBuffer{max: maxExecOutput}
	cmd.Stdout, cmd.Stderr = out, errBuf
	// CommandContext 只杀直接子进程；孙进程若继承 stdout 管道会让 Run() 挂到
	// 孙进程退出为止。WaitDelay 限定：进程被杀后管道仍被占用 → 等待 500ms 即
	// 强制关闭并返回（版本探测宁可截断不悬挂扫描循环）。
	cmd.WaitDelay = 500 * time.Millisecond
	err := cmd.Run()
	if vctx.Err() != nil {
		return "", trunc(fmt.Sprintf("version probe timeout (%s)", timeout), maxDetailLen)
	}
	if err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		detail := "version probe failed: " + firstLine(msg)
		if out.truncated || errBuf.truncated {
			detail += "; " + truncNote
		}
		return "", trunc(detail, maxDetailLen)
	}
	captured := out.String()
	v := firstLine(captured)
	lineComplete := firstLineComplete(captured)
	bufTruncated := out.truncated
	if v == "" {
		errStr := errBuf.String()
		v = firstLine(errStr)
		lineComplete = firstLineComplete(errStr)
		bufTruncated = errBuf.truncated
	}
	if v == "" {
		// 退出 0 但没有任何非空输出。若收集被封顶截断，「无输出」结论不可信
		// （真实版本行可能已被截掉）——报 unavailable + 截断说明，不编造
		// （R13-#4：stdout 空行+截断同样命中）。
		if out.truncated || errBuf.truncated {
			return "", trunc("version probe output incomplete: "+truncNote, maxDetailLen)
		}
		// 真的没有任何版本输出：探测成功但版本未知，如实留空。
		return "", ""
	}
	if bufTruncated && !lineComplete {
		// 版本值取自被封顶截断的缓冲、且选中行在已捕获字节内没有换行终止
		// （可能是被截断的半行）：不冒充版本号。按「实际选中的行」判定——
		// 版本值来自哪个缓冲就查哪个缓冲（R13-#4：stderr 截断同样命中）。
		return "", trunc("version probe output incomplete: "+truncNote+" before first line break", maxDetailLen)
	}
	return trunc(v, maxVersionLen), ""
}

// firstLineComplete 判定 firstLine(src) 实际选中的行是否在已捕获字节内以换行
// 终止（即该行完整、未被封顶截断吃掉换行；截断缓冲末尾无换行的行可能只是
// 半行）。行选取规则与 firstLine 完全一致（逐行 sanitize+TrimSpace 后取首个
// 非空行），保证两函数看到的是同一行。
func firstLineComplete(src string) bool {
	parts := strings.Split(src, "\n")
	for i, ln := range parts {
		if strings.TrimSpace(sanitize(ln)) != "" {
			return i < len(parts)-1
		}
	}
	return false
}

func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(sanitize(ln))
		if ln != "" {
			return ln
		}
	}
	return ""
}

// sanitize 去控制字符（版本串/详情入库前有界、可安全展示）。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func trunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// rune 边界安全截断。
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
