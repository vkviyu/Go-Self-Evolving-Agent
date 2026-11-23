//go:build darwin || linux

package codingtools

import (
	"os"
	"syscall"
)

// A path can be replaced with a named pipe between validation and open.
func openRootFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
