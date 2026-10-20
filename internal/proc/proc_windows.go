package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached starts the command with no console (DETACHED_PROCESS) in a
// new process group, so console Ctrl+C/close events of the agent's
// terminal don't reach it.
//
// It first asks to break away from the caller's job object: an agent may
// run hooks inside a job that kills every process in it when the hook
// exits. If the job forbids breakaway, it starts the process without it.
func startDetached(newCmd func() *exec.Cmd) (*os.Process, error) {
	const flags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
	cmd := newCmd()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB, HideWindow: true}
	err := cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
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
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means it exists but belongs to someone else.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
