//go:build !windows && !darwin

package notify

import (
	"errors"
	"os/exec"
)

// desktopCommand shows a notification through libnotify's notify-send,
// where it is installed.
func desktopCommand(title, body string) (command, error) {
	p, err := lookPath("notify-send")
	if err != nil {
		return command{}, errors.New("notify-send not found (install libnotify, e.g. the libnotify-bin package)")
	}
	return command{name: p, args: []string{"--app-name=LoopGuard", "--", title, body}}, nil
}

func hideWindow(*exec.Cmd) {}
