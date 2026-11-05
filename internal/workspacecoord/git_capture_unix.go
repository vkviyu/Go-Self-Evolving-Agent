//go:build darwin || linux

package workspacecoord

import (
	"os"
	"syscall"
)

// Nonblocking open prevents an Agent-created FIFO from hanging the host before
// the descriptor's regular-file check. os.Root enforces symlink confinement.
func openGitDataFile(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
