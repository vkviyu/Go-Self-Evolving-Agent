//go:build darwin || linux

package workspacedomain

import (
	"fmt"
	"os"
	"syscall"
)

// directoryKey uses the device and inode numbers, which survive renames and
// are shared by every path that reaches the same directory.
func directoryKey(canonical string, info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "path:" + canonical
	}
	return fmt.Sprintf("dev:%d:ino:%d", uint64(stat.Dev), uint64(stat.Ino))
}
