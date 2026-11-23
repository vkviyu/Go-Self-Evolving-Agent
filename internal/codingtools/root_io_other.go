//go:build !darwin && !linux

package codingtools

import "os"

func openRootFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
