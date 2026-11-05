//go:build !darwin && !linux

package sandbox

import "os/exec"

func runProcessGroup(*exec.Cmd) error { return ErrUnavailable }
