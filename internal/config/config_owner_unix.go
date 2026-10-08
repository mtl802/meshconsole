//go:build unix

package config

import (
	"fmt"
	"os"
	"syscall"
)

// checkConfigPlatformSecurity 为 CheckConfigFileSecurity 的 unix 平台钩子
// （SPEC §4，口径与 M1b-c2 首版一致不变）：0600 + 属运行用户。
func checkConfigPlatformSecurity(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config %s: %w", path, err)
	}
	if p := fi.Mode().Perm(); p != 0o600 {
		return fmt.Errorf("公网模式启动被拒绝：config 文件权限为 %o（公网形态要求 0600，chmod 600 %s 后重试）", p, path)
	}
	return fileOwnedByCurrentUser(path)
}

// fileOwnedByCurrentUser 校验文件属主为当前运行用户（公网模式门槛，SPEC §4）。
// 仅 unix 有可移植的属主语义（Stat_t.Uid）；Windows 侧由同名词源文件跳过。
func fileOwnedByCurrentUser(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return nil // 平台未提供属主信息：不误报，其余两道检查已覆盖
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("公网模式启动被拒绝：config 文件 %s 不属于运行用户（uid %d ≠ %d）；chown 后重试",
			path, st.Uid, os.Geteuid())
	}
	return nil
}
