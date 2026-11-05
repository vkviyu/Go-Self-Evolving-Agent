//go:build darwin || linux

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// A canceled sandbox must not leave compiler or shell descendants writing
// after the owner removes its private directory or releases a workspace lease.
func runProcessGroup(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return err
	}
	defer syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	return command.Wait()
}
