package notify

import (
	"encoding/base64"
	"os/exec"
	"syscall"
	"unicode/utf16"
)

// toastScript shows a toast with the title and body from the environment.
// It runs under Windows PowerShell's app id, which Windows already knows:
// an app id of LoopGuard's own would have to be registered first (a Start
// menu shortcut), so the toast is labeled "Windows PowerShell". The text
// is XML-escaped in the script; it never becomes script code.
const toastScript = `$ErrorActionPreference = 'Stop'
[void][Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
[void][Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime]
$t = [Security.SecurityElement]::Escape($env:LOOPGUARD_TOAST_TITLE)
$b = [Security.SecurityElement]::Escape($env:LOOPGUARD_TOAST_BODY)
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml('<toast><visual><binding template="ToastGeneric"><text>' + $t + '</text><text>' + $b + '</text></binding></visual></toast>')
$app = '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe'
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($app).Show([Windows.UI.Notifications.ToastNotification]::new($x))
`

func desktopCommand(title, body string) (command, error) {
	ps, err := lookPath("powershell.exe")
	if err != nil {
		return command{}, err
	}
	u := utf16.Encode([]rune(toastScript))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return command{
		name: ps,
		args: []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(b)},
		env:  []string{"LOOPGUARD_TOAST_TITLE=" + title, "LOOPGUARD_TOAST_BODY=" + body},
	}, nil
}

// createNoWindow keeps PowerShell from opening a console window: the
// daemon runs detached, so a console child would get a new one.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
