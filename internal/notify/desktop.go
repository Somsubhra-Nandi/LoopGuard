package notify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
)

// Desktop shows a desktop notification with the platform's own tool, so
// LoopGuard needs no extra dependency: a Windows toast through Windows
// PowerShell, Notification Center through osascript on macOS, and
// notify-send (libnotify) elsewhere, when installed. The title and body
// are passed as data (environment variables or arguments), never spliced
// into a script.
type Desktop struct{}

// Send implements Sender.
func (Desktop) Send(ctx context.Context, _ config.Channel, n Notification) error {
	c, err := desktopCommand(n.Title(), n.Body())
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	cmd.Env = append(os.Environ(), c.env...)
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("%s: %v %s", filepath.Base(c.name), err, msg)
	}
	return nil
}

// DesktopAvailable reports whether desktop notifications can be shown
// here, and how (for doctor).
func DesktopAvailable() (bool, string) {
	c, err := desktopCommand("", "")
	if err != nil {
		return false, err.Error()
	}
	return true, "through " + filepath.Base(c.name)
}

// command is a program to run with arguments and extra environment.
type command struct {
	name string
	args []string
	env  []string
}

// lookPath is exec.LookPath (tests replace it).
var lookPath = exec.LookPath
