package notify

import "os/exec"

// desktopCommand shows a Notification Center banner. The title and body
// are osascript arguments (argv), never part of the script.
func desktopCommand(title, body string) (command, error) {
	p, err := lookPath("osascript")
	if err != nil {
		return command{}, err
	}
	return command{name: p, args: []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		title, body,
	}}, nil
}

func hideWindow(*exec.Cmd) {}
