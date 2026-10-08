//go:build windows

package collect

import (
	"os"
	"syscall"
)

// fileBirthTime 返回文件的创建时刻（unix 秒）。Windows 的 Win32FileAttributeData
// 携带真实 CreationTime（FILETIME：1601 起 100ns 计数）——会话文件创建于会话
// 开始，即「会话开始时刻」。取不到时回退 mtime（持续时长为下界近似，不编造
// 更早的起点）。
func fileBirthTime(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		ft := int64(st.CreationTime.HighDateTime)<<32 | int64(st.CreationTime.LowDateTime)
		if ft > 0 {
			return ft/1e7 - 11644473600
		}
	}
	return fi.ModTime().Unix()
}
