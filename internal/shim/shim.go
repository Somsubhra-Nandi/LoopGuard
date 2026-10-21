package shim

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/home/homedir"
	"github.com/Somsubhra-Nandi/loopguard/internal/proc"
)

// The shim runs once per hook event, so its start-up time is the hook's
// overhead. It imports no fmt, encoding/json, net/http, or crypto: the
// request is a hand-written HTTP/1.0 POST, and the reply is decoded by
// decodeReply. `go list -deps ./cmd/loopguard-hook` shows what it pulls in.

// Environment variables the shim reads.
const (
	EnvDisable     = "LOOPGUARD_DISABLE"         // 1: do nothing
	EnvTimeoutMS   = "LOOPGUARD_HOOK_TIMEOUT_MS" // lowers the 800 ms timeout
	EnvDebug       = "LOOPGUARD_DEBUG"           // 1: explain failures on stderr
	EnvNoAutostart = "LOOPGUARD_NO_AUTOSTART"    // 1: never start the daemon
)

// Limits (LLD §10).
const (
	MaxPayload       = 4 << 20
	MaxTimeout       = 800 * time.Millisecond
	AutostartEvery   = 10 * time.Second
	maxReply         = 1 << 20
	maxHeader        = 16 << 10
	autostartLock    = "autostart.lock"
	timeoutHeader    = "X-LoopGuard-Timeout-Ms" // = daemon.TimeoutHeader
	defaultAgentName = "claude-code"
)

// Options configures one hook invocation. Zero values take the defaults.
type Options struct {
	Agent  string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Home   string              // "" → homedir.Dir()
	Getenv func(string) string // nil → os.Getenv
	// Spawn starts the daemon in the background without waiting for it.
	// nil → SpawnDaemon.
	Spawn func(home string) error
}

// Run handles one hook event and returns the process exit code (LLD §10):
// forward the payload on stdin to the daemon, print the decision it
// returns exactly, and exit with its code. Any failure (no daemon, a slow
// or broken one, bad input, a panic) prints nothing and exits 0, so the
// agent carries on as if LoopGuard weren't there. If the daemon isn't
// running, Run starts it (at most once per 10 s) and exits at once; that
// event is lost.
func Run(o Options) (code int) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	debug := func(msg string) {
		if getenv(EnvDebug) == "1" && o.Stderr != nil {
			io.WriteString(o.Stderr, "loopguard hook: "+msg+"\n")
		}
	}
	defer func() {
		if r := recover(); r != nil {
			msg := "panic"
			switch v := r.(type) {
			case string:
				msg += ": " + v
			case error:
				msg += ": " + v.Error()
			}
			debug(msg)
			code = 0
		}
	}()

	if getenv(EnvDisable) == "1" {
		drain(o.Stdin)
		return 0
	}
	body, err := readPayload(o.Stdin)
	if err != nil {
		debug(err.Error())
		return 0
	}

	dir := o.Home
	if dir == "" {
		if dir, err = homedir.Dir(); err != nil {
			debug("home directory: " + err.Error())
			return 0
		}
	}
	port, err := homedir.ReadPort(dir)
	if err != nil {
		debug("daemon not running (" + err.Error() + ")")
		autostart(dir, o, getenv, debug)
		return 0
	}
	token, err := homedir.ReadToken(dir)
	if err != nil {
		debug("token: " + err.Error())
		return 0
	}

	agent := o.Agent
	if agent == "" {
		agent = defaultAgentName
	}
	timeout := hookTimeout(getenv(EnvTimeoutMS))
	path := "/v1/events?agent=" + queryEscape(agent)
	// Whether the session is interactive (terminal notifications): Claude
	// Code tells its hooks how it was started.
	for _, p := range [][2]string{{"entry", "CLAUDE_CODE_ENTRYPOINT"}, {"attended", "CLAUDE_CODE_SESSION_ATTENDED"}} {
		if v := getenv(p[1]); v != "" {
			path += "&" + p[0] + "=" + queryEscape(v)
		}
	}
	status, reply, err := post(port, path, token, body, timeout)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			// Nothing listens: daemon.json is stale (a crashed daemon).
			debug("daemon unreachable on port " + strconv.Itoa(port) + " (" + err.Error() + ")")
			autostart(dir, o, getenv, debug)
			return 0
		}
		debug("request: " + err.Error())
		return 0
	}
	if status != 200 {
		debug("daemon replied " + strconv.Itoa(status))
		return 0
	}
	stdout, exit, err := decodeReply(reply)
	if err != nil {
		debug("bad reply: " + err.Error())
		return 0
	}
	if exit < 0 || exit > 255 {
		debug("bad exit code " + strconv.Itoa(exit) + " in reply")
		return 0
	}
	if stdout != "" {
		if _, err := io.WriteString(o.Stdout, stdout); err != nil {
			debug("writing stdout: " + err.Error())
			return 0
		}
	}
	return exit
}

// post sends one HTTP/1.0 request to 127.0.0.1:port and returns the status
// code and body of the reply, all within timeout. HTTP/1.0 keeps the reply
// simple: never chunked, and the connection closes after it. The proxy
// environment variables are never consulted.
func post(port int, path, token string, body []byte, timeout time.Duration) (int, []byte, error) {
	deadline := time.Now().Add(timeout)
	conn, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, nil, err
	}
	req := make([]byte, 0, 256+len(body))
	req = append(req, "POST "+path+" HTTP/1.0\r\n"+
		"Host: 127.0.0.1:"+strconv.Itoa(port)+"\r\n"+
		"Authorization: Bearer "+token+"\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: "+strconv.Itoa(len(body))+"\r\n"+
		timeoutHeader+": "+strconv.FormatInt(timeout.Milliseconds(), 10)+"\r\n\r\n"...)
	req = append(req, body...)
	if _, err := conn.Write(req); err != nil {
		return 0, nil, err
	}
	resp, err := io.ReadAll(io.LimitReader(conn, maxHeader+maxReply+1))
	if err != nil {
		return 0, nil, err
	}
	if len(resp) > maxHeader+maxReply {
		return 0, nil, errors.New("reply too large")
	}
	return parseResponse(resp)
}

// parseResponse splits an HTTP/1.x response into its status code and body,
// cutting the body at Content-Length when there is one.
func parseResponse(resp []byte) (int, []byte, error) {
	s := string(resp)
	head, body, ok := strings.Cut(s, "\r\n\r\n")
	if !ok {
		return 0, nil, errors.New("truncated reply")
	}
	statusLine, headers, _ := strings.Cut(head, "\r\n")
	proto, rest, _ := strings.Cut(statusLine, " ")
	if !strings.HasPrefix(proto, "HTTP/1.") || len(rest) < 3 {
		return 0, nil, errors.New("not an HTTP reply")
	}
	status, err := strconv.Atoi(rest[:3])
	if err != nil {
		return 0, nil, errors.New("bad status line")
	}
	for _, h := range strings.Split(headers, "\r\n") {
		name, val, _ := strings.Cut(h, ":")
		val = strings.TrimSpace(val)
		switch {
		case strings.EqualFold(name, "Transfer-Encoding") && !strings.EqualFold(val, "identity"):
			return 0, nil, errors.New("unexpected Transfer-Encoding " + val)
		case strings.EqualFold(name, "Content-Length"):
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > len(body) {
				return 0, nil, errors.New("body shorter than Content-Length")
			}
			body = body[:n]
		}
	}
	return status, []byte(body), nil
}

// queryEscape percent-encodes everything but unreserved characters.
func queryEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// readPayload reads stdin up to MaxPayload. A larger payload is dropped
// (forwarding a cut JSON document would be useless), but stdin is still
// read to the end so the agent's write doesn't fail.
func readPayload(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, errors.New("no stdin")
	}
	body, err := io.ReadAll(io.LimitReader(r, MaxPayload+1))
	if err != nil {
		return nil, errors.New("reading stdin: " + err.Error())
	}
	if len(body) > MaxPayload {
		drain(r)
		return nil, errors.New("payload over " + strconv.Itoa(MaxPayload) + " bytes; skipped")
	}
	return body, nil
}

func drain(r io.Reader) {
	if r != nil {
		_, _ = io.Copy(io.Discard, r)
	}
}

// hookTimeout is min(800 ms, LOOPGUARD_HOOK_TIMEOUT_MS).
func hookTimeout(env string) time.Duration {
	t := MaxTimeout
	if ms, err := strconv.Atoi(env); err == nil && ms > 0 && time.Duration(ms)*time.Millisecond < t {
		t = time.Duration(ms) * time.Millisecond
	}
	return t
}

// autostart starts the daemon unless one was started in the last 10 s
// (by any hook process: the lock file's mtime records the last attempt).
// Two hooks racing past the check may both spawn; the daemon's own
// single-instance check makes the second one exit.
func autostart(dir string, o Options, getenv func(string) string, debug func(string)) {
	if getenv(EnvNoAutostart) == "1" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		debug("autostart: " + err.Error())
		return
	}
	lock := filepath.Join(dir, autostartLock)
	now := time.Now()
	if fi, err := os.Stat(lock); err == nil {
		if age := now.Sub(fi.ModTime()); age >= 0 && age < AutostartEvery {
			debug("autostart: attempted " + age.Round(time.Millisecond).String() + " ago; skipping")
			return
		}
		if err := os.Chtimes(lock, now, now); err != nil {
			debug("autostart: " + err.Error())
			return
		}
	} else {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			debug("autostart: " + err.Error()) // most likely another hook just created it
			return
		}
		f.Close()
	}
	spawn := o.Spawn
	if spawn == nil {
		spawn = SpawnDaemon
	}
	if err := spawn(dir); err != nil {
		debug("autostart: " + err.Error())
		return
	}
	debug("autostart: daemon starting")
}

// HookBinary is the base name of the dedicated hook executable (without
// the .exe on Windows).
const HookBinary = "loopguard-hook"

// SpawnDaemon starts `loopguard daemon` fully detached, in the LoopGuard
// home directory. The daemon is this executable, or, when this is
// loopguard-hook, the loopguard executable next to it.
func SpawnDaemon(dir string) error {
	exe, err := DaemonExecutable()
	if err != nil {
		return err
	}
	return proc.SpawnDetached(dir, exe, "daemon")
}

// DaemonExecutable returns the path of the loopguard executable that runs
// the daemon (see SpawnDaemon).
func DaemonExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(exe)
	if !strings.EqualFold(strings.TrimSuffix(filepath.Base(exe), ext), HookBinary) {
		return exe, nil
	}
	sibling := filepath.Join(filepath.Dir(exe), "loopguard"+ext)
	if _, err := os.Stat(sibling); err != nil {
		return "", errors.New("no loopguard executable next to " + exe)
	}
	return sibling, nil
}
