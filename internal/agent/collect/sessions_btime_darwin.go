//go:build darwin

package collect

import (
	"os"
	"syscall"
)

// fileBirthTime 返回文件的创建时刻（unix 秒）。darwin 的 Stat_t 携带真实创建
// 时刻（Birthtim）——会话 JSONL 文件创建于会话开始，即「会话开始时刻」。
// 取不到时回退 mtime（面板持续时长为下界近似，不编造更早的起点）。
func fileBirthTime(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Birthtimespec.Sec > 0 {
		return st.Birthtimespec.Sec
	}
	return fi.ModTime().Unix()
}
