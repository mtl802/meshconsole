//go:build linux

package collect

import (
	"os"
	"syscall"
)

// fileBirthTime 返回文件的创建时刻（unix 秒）。Linux 的 Stat_t 无通用创建时刻
// 字段（crtime 须走 statx），以 Ctim 近似——ext4 等常见文件系统上 Ctim 随写入
// 元数据更新，长会话的持续时长在该平台为下界近似（宁短不编造）。
// 取不到时回退 mtime。
func fileBirthTime(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Ctim.Sec > 0 {
		return st.Ctim.Sec
	}
	return fi.ModTime().Unix()
}
