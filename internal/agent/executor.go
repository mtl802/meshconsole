// executor.go 为 agent 侧白名单命令执行器（SPEC-M1d §3 执行环境约束）：
//   - 固定最小 env（PATH 白名单目录、SYSTEMD_PAGER=cat 禁 pager），不继承
//     agent 自身环境（防 PATH 替换/环境注入）；
//   - 固定工作目录（state 同目录，绝不使用进程默认 cwd）；
//   - 固定 argv 直接 exec（cmdkind.BuildArgv 产物），不经 shell——单横线选项
//     是模板固定部分，参数槽只填占位；
//   - timeout（默认 30s 上限 300s）+ 进程组 kill（unix Setpgid 后 kill(-pgid)，
//     覆盖子进程树；Windows 无 kind 实装，退化为单进程 Kill 并如实标注）；
//   - stdout/stderr 合并采集 64KB 硬上限（超出截断并标注；**标注占用 64KB
//     预算内**——console 对回执执行同等硬校验，超限回执会被整体拒绝）。
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// ResultHardCap 为执行输出采集硬上限（SPEC-M1d §3：64KB，与 console 回执上限
// 一致——最终回执文本总长（含标注）不得超出，绝不整段丢弃）。
const ResultHardCap = 64 << 10

// truncNoteBody / truncNoteTimeout 为截断/超时标注（占用 64KB 预算内）。
const (
	truncNoteBody    = "\n[meshagent] 输出超过 64KB 上限，已截断"
	truncNoteTimeout = "\n[meshagent] 执行超时（%ds），已终止进程组"
)

// fixedEnv 为执行环境的固定最小集合（SPEC §3：PATH 白名单目录 + 禁 pager）。
// TERM=dumb 关闭颜色/分页交互；launchctl/log 不需要 HOME 等用户环境。
func fixedEnv() []string {
	path := "/usr/bin:/bin:/usr/sbin:/sbin"
	if runtime.GOOS == "windows" {
		path = `C:\Windows\System32`
	}
	return []string{
		"PATH=" + path,
		"SYSTEMD_PAGER=cat",
		"PAGER=cat",
		"TERM=dumb",
	}
}

// errExecTimeout 供 worker/测试识别超时终止路径。
var errExecTimeout = errors.New("execution timed out")

// Execute 执行一条已通过校验的 argv（timeout 上限 300s 由 worker 保证）。
// 返回退出码（nil = 信号终止/超时/启动失败）与合并输出（总长 ≤64KB）。
func Execute(ctx context.Context, argv []string, timeoutS int64, cwd string) (*int64, string, error) {
	if len(argv) == 0 {
		return nil, "", errors.New("empty argv")
	}
	if timeoutS < 1 {
		timeoutS = 30
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS)*time.Second)
	defer cancel()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = fixedEnv()
	cmd.Dir = cwd
	// 合并采集 stdout/stderr（SPEC：stdout/stderr 合并采集）。
	var buf limitedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// 进程组：unix Setpgid 使 kill(-pgid) 覆盖整棵子进程树（平台实现在
	// executor_unix.go / executor_windows.go——Windows 无 kind 实装，SPEC §6）。
	setPgid(cmd)
	if err := cmd.Start(); err != nil {
		return nil, finalizeResult(buf.String(), buf.truncated, ""), fmt.Errorf("exec %s: %w", argv[0], err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-tctx.Done():
		// 进程组 kill（unix）：负 PID 命中整个组，后代无法逃逸（DESIGN §4.2-C
		// 的 L1 版口径；命令 ≤300s 只读白名单，无 cgroup 亦无写面）。
		killGroup(cmd)
		<-done // 回收僵尸
		note := fmt.Sprintf(truncNoteTimeout, timeoutS)
		return nil, finalizeResult(buf.String(), buf.truncated, note), errExecTimeout
	case err := <-done:
		text := finalizeResult(buf.String(), buf.truncated, "")
		if err == nil {
			zero := int64(0)
			return &zero, text, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code := int64(ee.ExitCode())
			return &code, text, nil
		}
		// 启动后非退出类错误（罕见）：如实失败。
		return nil, text, err
	}
}

// limitedBuffer 为 64KB 硬上限的合并采集缓冲：触顶后丢弃后续写入（进程输出
// 不因停止读而阻塞——pipe 缓冲耗尽后内核会阻塞子进程写端，属预期背压；上限
// 内命令本就 ≤300s）。
type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > ResultHardCap {
		room := ResultHardCap - b.buf.Len()
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil // 吞掉溢出部分：写端永远成功
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// finalizeResult 回执收尾，**最后一步硬保证总长 ≤ ResultHardCap**（R39-#3 收口
// ——console 对回执执行同等 64KB 硬校验，超限即整体拒收、命令停 claimed）。
// 顺序即防线：① UTF-8 替换最先做（非法序列→U+FFFD 会扩容，替换后再预算）；②
// 标注计入预算，正文超预算先压正文、保住标注（并如实补截断说明）；③ 截断按
// rune 边界切（不制造新的非法序列，JSON 序列化不扩容）。任何可能扩容的操作都
// 发生在预算复核之前，出口处长度由构造保证不超限。
func finalizeResult(body string, already bool, note string) string {
	suffix := validUTF8(note)
	if already {
		suffix += truncNoteBody
	}
	s := validUTF8(body)
	if len(s)+len(suffix) > ResultHardCap {
		// 先把将追加的截断说明并入 suffix、再算正文预算（R39-#3 缺陷本体：
		// 先按旧 suffix 压正文、后追加标注，总长恰好再超 cap 一次）。
		if !strings.Contains(suffix, truncNoteBody) {
			suffix += truncNoteBody
		}
		if len(suffix) > ResultHardCap {
			// 防御分支（标注来自固定常量+错误串，正常触不到）：标注自身也须
			// 让位硬上限，正文清空。
			suffix = cutValidUTF8(suffix, ResultHardCap)
		}
		s = cutValidUTF8(s, ResultHardCap-len(suffix))
	}
	if suffix == "" {
		return s
	}
	return s + suffix
}

// cutValidUTF8 把合法 UTF-8 字符串截到 ≤max 字节且保持合法（回退到 rune 边界）。
// 输入须已过 validUTF8——边界回退仅在合法序列上才保持合法性。
func cutValidUTF8(s string, max int) string {
	if max >= len(s) {
		return s
	}
	if max < 0 {
		max = 0
	}
	for max > 0 && !isRuneStartByte(s[max]) {
		max--
	}
	return s[:max]
}

func isRuneStartByte(b byte) bool { return b&0xC0 != 0x80 }

func validUTF8(s string) string {
	if !utf8.ValidString(s) {
		return strings.ToValidUTF8(s, "\uFFFD")
	}
	return s
}
