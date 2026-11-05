//go:build !darwin && !linux

package workspacecoord

import (
	"errors"
	"os"
)

func openGitDataFile(*os.Root, string) (*os.File, error) {
	return nil, errors.New("protected Git metadata capture is unavailable on this platform")
}
