//go:build unix

// executor_unix.go：unix 形态的进程组控制（SPEC-M1d §3 超时进程组 kill）。
package agent

import (
	"os/exec"
	"syscall"
)

// setPgid 让子进程自成进程组（Setpgid），超时 kill(-pgid) 可覆盖整棵树。
func setPgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup 对子进程所在进程组发 SIGKILL（负 PID 命中全组，后代无法逃逸）。
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
