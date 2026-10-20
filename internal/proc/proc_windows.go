package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Process creation flags and access rights missing from package syscall.
// (The hook binary imports this package, so it sticks to syscall rather
// than golang.org/x/sys/windows.)
const (
	detachedProcess                = 0x00000008
	createBreakawayFromJob         = 0x01000000
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// startDetached starts the command with no console (DETACHED_PROCESS) in a
// new process group, so console Ctrl+C/close events of the agent's
// terminal don't reach it.
//
// It first asks to break away from the caller's job object: an agent may
// run hooks inside a job that kills every process in it when the hook
// exits. If the job forbids breakaway, it starts the process without it.
func startDetached(newCmd func() *exec.Cmd) (*os.Process, error) {
	const flags = detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP
	cmd := newCmd()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags | createBreakawayFromJob, HideWindow: true}
	err := cmd.Start()
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		cmd = newCmd()
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		err = cmd.Start()
	}
	if err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// Alive reports whether a process with this pid is running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Access denied means it exists but belongs to someone else.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
