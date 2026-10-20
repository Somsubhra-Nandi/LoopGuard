package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/proc"
)

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
	Home   string              // "" → home.Dir()
	Getenv func(string) string // nil → os.Getenv
	// Spawn starts the daemon in the background without waiting for it.
	// nil → start this executable's `daemon` command, detached.
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
	debug := func(format string, args ...any) {
		if getenv(EnvDebug) == "1" && o.Stderr != nil {
			fmt.Fprintf(o.Stderr, "loopguard hook: "+format+"\n", args...)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			debug("panic: %v", r)
			code = 0
		}
	}()

	if getenv(EnvDisable) == "1" {
		drain(o.Stdin)
		return 0
	}
	body, err := readPayload(o.Stdin)
	if err != nil {
		debug("%v", err)
		return 0
	}

	dir := o.Home
	if dir == "" {
		if dir, err = home.Dir(); err != nil {
			debug("home directory: %v", err)
			return 0
		}
	}
	info, err := home.ReadDaemonInfo(dir)
	if err != nil {
		debug("daemon not running (%v)", err)
		autostart(dir, o, getenv, debug)
		return 0
	}
	token, err := home.ReadToken(dir)
	if err != nil {
		debug("token: %v", err)
		return 0
	}

	agent := o.Agent
	if agent == "" {
		agent = defaultAgentName
	}
	timeout := hookTimeout(getenv(EnvTimeoutMS))
	u := fmt.Sprintf("http://127.0.0.1:%d/v1/events?agent=%s", info.Port, url.QueryEscape(agent))
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		debug("%v", err)
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(timeoutHeader, strconv.FormatInt(timeout.Milliseconds(), 10))

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil, // never route localhost through HTTP(S)_PROXY
			DisableKeepAlives: true,
			DialContext:       (&net.Dialer{Timeout: timeout}).DialContext,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			// Nothing listens: daemon.json is stale (a crashed daemon).
			debug("daemon unreachable on port %d (%v)", info.Port, err)
			autostart(dir, o, getenv, debug)
			return 0
		}
		debug("request: %v", err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		debug("daemon replied %s", resp.Status)
		return 0
	}
	var rep struct {
		Stdout string `json:"stdout"`
		Exit   int    `json:"exit"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReply)).Decode(&rep); err != nil {
		debug("bad reply: %v", err)
		return 0
	}
	if rep.Exit < 0 || rep.Exit > 255 {
		debug("bad exit code %d in reply", rep.Exit)
		return 0
	}
	if rep.Stdout != "" {
		if _, err := io.WriteString(o.Stdout, rep.Stdout); err != nil {
			debug("writing stdout: %v", err)
			return 0
		}
	}
	return rep.Exit
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
		return nil, fmt.Errorf("reading stdin: %w", err)
	}
	if len(body) > MaxPayload {
		drain(r)
		return nil, fmt.Errorf("payload over %d bytes; skipped", MaxPayload)
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
func autostart(dir string, o Options, getenv func(string) string, debug func(string, ...any)) {
	if getenv(EnvNoAutostart) == "1" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		debug("autostart: %v", err)
		return
	}
	lock := filepath.Join(dir, autostartLock)
	now := time.Now()
	if fi, err := os.Stat(lock); err == nil {
		if age := now.Sub(fi.ModTime()); age >= 0 && age < AutostartEvery {
			debug("autostart: attempted %v ago; skipping", age.Round(time.Millisecond))
			return
		}
		if err := os.Chtimes(lock, now, now); err != nil {
			debug("autostart: %v", err)
			return
		}
	} else {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			debug("autostart: %v", err) // most likely another hook just created it
			return
		}
		f.Close()
	}
	spawn := o.Spawn
	if spawn == nil {
		spawn = spawnDaemon
	}
	if err := spawn(dir); err != nil {
		debug("autostart: %v", err)
		return
	}
	debug("autostart: daemon starting")
}

// spawnDaemon starts `<this executable> daemon` fully detached, in the
// LoopGuard home directory.
func spawnDaemon(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return proc.SpawnDetached(dir, exe, "daemon")
}
