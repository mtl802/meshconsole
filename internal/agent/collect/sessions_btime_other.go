//go:build !darwin && !linux && !windows

package collect

import (
	"os"
)

// fileBirthTime 返回文件的创建时刻（unix 秒）。其余 unix 平台无统一创建时刻
// 接口，回退 mtime（持续时长为下界近似，不编造更早的起点）。
func fileBirthTime(fi os.FileInfo) int64 {
	return fi.ModTime().Unix()
}
