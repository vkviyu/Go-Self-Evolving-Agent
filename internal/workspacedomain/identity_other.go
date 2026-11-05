//go:build !darwin && !linux

package workspacedomain

import "os"

// directoryKey falls back to the canonical path where inode identity is
// unavailable; aliases that are not resolved by EvalSymlinks stay distinct.
func directoryKey(canonical string, _ os.FileInfo) string {
	return "path:" + canonical
}
