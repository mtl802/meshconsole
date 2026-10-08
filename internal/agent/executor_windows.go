//go:build windows

// executor_windows.go：Windows 无 kind 实装（SPEC-M1d §6 非目标，caps 预留
// windows-*）；进程组语义如实退化为单进程 Kill——Job Object 杀树属 M3b。
package agent

import "os/exec"

func setPgid(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
