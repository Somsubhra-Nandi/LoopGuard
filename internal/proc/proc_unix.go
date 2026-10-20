//go:build !windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts the command in a new session (setsid), detached
// from the agent's terminal and process group.
func startDetached(newCmd func() *exec.Cmd) (*os.Process, error) {
	cmd := newCmd()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// Alive reports whether a process with this pid is running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
