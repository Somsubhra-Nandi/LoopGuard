// Package homedir is the dependency-free core of package home: where the
// LoopGuard home is, the names of the files in it, and readers for the two
// files the hook shim needs (the daemon's port and the API token).
//
// It imports only os, path/filepath, strconv, and strings, so the hook
// binary (cmd/loopguard-hook) stays small and starts fast. Package home
// re-exports everything here.
package homedir

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// EnvHome overrides the data directory.
const EnvHome = "LOOPGUARD_HOME"

// File names inside the data directory.
const (
	DaemonFile  = "daemon.json"
	TokenFile   = "token"
	DBFile      = "loopguard.db"
	LogDir      = "logs"
	CaptureDir  = "capture"
	CaptureFlag = "capture.on" // capture mode is on while this file exists
	BinDir      = "bin"
)

// TokenBytes is the size of the random API token (hex-encoded in the file).
const TokenBytes = 32

// Dir returns LoopGuard's data directory: $LOOPGUARD_HOME, or .loopguard in
// the user's home directory (%USERPROFILE%\.loopguard on Windows).
func Dir() (string, error) {
	if h := os.Getenv(EnvHome); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".loopguard"), nil
}

// ReadToken reads the API token from <dir>/token.
func ReadToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, TokenFile))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) != 2*TokenBytes {
		return "", errors.New(TokenFile + ": malformed token")
	}
	return tok, nil
}

// ReadPort reads the daemon's port from <dir>/daemon.json without a JSON
// decoder: it finds the "port" member and parses its number. The file is
// written by the daemon (package home), so its shape is known.
func ReadPort(dir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dir, DaemonFile))
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.Index(s, `"port"`)
	if i < 0 {
		return 0, errors.New(DaemonFile + ": no port")
	}
	s = strings.TrimLeft(s[i+len(`"port"`):], " \t\r\n")
	if !strings.HasPrefix(s, ":") {
		return 0, errors.New(DaemonFile + ": malformed port")
	}
	s = strings.TrimLeft(s[1:], " \t\r\n")
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	port, err := strconv.Atoi(s[:n])
	if err != nil || port <= 0 || port > 65535 {
		return 0, errors.New(DaemonFile + ": invalid port " + strconv.Quote(s[:n]))
	}
	return port, nil
}
