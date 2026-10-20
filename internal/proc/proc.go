// Package proc holds the OS-specific process helpers the daemon and the
// hook shim share: starting a fully detached background process, and
// checking whether a pid is alive.
package proc

import (
	"os"
	"os/exec"
)

// SpawnDetached starts exe with args as a background process that outlives
// the caller: no console window, no inherited stdio (it gets the null
// device), its own process group or session, and no waiting. It runs in
// dir, so it doesn't keep the caller's working directory (an agent's
// project) busy. A nil error means only that the process was created.
func SpawnDetached(dir, exe string, args ...string) error {
	p, err := startDetached(func() *exec.Cmd {
		// Nil Stdin/Stdout/Stderr: the child gets the null device, so it
		// holds none of the agent's pipes open.
		cmd := exec.Command(exe, args...)
		cmd.Dir = dir
		return cmd
	})
	if err != nil {
		return err
	}
	// Don't wait: the child is on its own.
	return p.Release()
}

// Kill terminates the process with this pid.
func Kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
