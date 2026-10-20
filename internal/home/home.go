// Package home locates LoopGuard's data directory and the small files the
// daemon and the hook shim share through it: daemon.json (where the daemon
// listens) and the API token.
package home

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/home/homedir"
)

// Shared with the hook shim through package homedir.
const (
	EnvHome     = homedir.EnvHome
	DaemonFile  = homedir.DaemonFile
	TokenFile   = homedir.TokenFile
	DBFile      = homedir.DBFile
	LogDir      = homedir.LogDir
	CaptureDir  = homedir.CaptureDir
	CaptureFlag = homedir.CaptureFlag
	BinDir      = homedir.BinDir
)

// Dir returns LoopGuard's data directory: $LOOPGUARD_HOME, or .loopguard in
// the user's home directory (%USERPROFILE%\.loopguard on Windows).
func Dir() (string, error) { return homedir.Dir() }

// DaemonInfo is the content of daemon.json, written by a running daemon.
type DaemonInfo struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// ReadDaemonInfo reads <dir>/daemon.json.
func ReadDaemonInfo(dir string) (DaemonInfo, error) {
	var info DaemonInfo
	b, err := os.ReadFile(filepath.Join(dir, DaemonFile))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, fmt.Errorf("%s: %w", DaemonFile, err)
	}
	if info.Port <= 0 || info.Port > 65535 {
		return info, fmt.Errorf("%s: invalid port %d", DaemonFile, info.Port)
	}
	return info, nil
}

// WriteDaemonInfo writes <dir>/daemon.json atomically, so a reader never
// sees a half-written file.
func WriteDaemonInfo(dir string, info DaemonInfo) error {
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, DaemonFile), b)
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// tokenBytes is the size of the random API token.
const tokenBytes = homedir.TokenBytes

// ReadToken reads the API token from <dir>/token.
func ReadToken(dir string) (string, error) { return homedir.ReadToken(dir) }

// EnsureToken returns the API token, creating <dir>/token with a random
// 32-byte (hex-encoded) token on first use. The file is readable only by
// the current user: mode 0600, and on Windows an ACL that grants access to
// the current user alone.
func EnsureToken(dir string) (string, error) {
	if tok, err := ReadToken(dir); err == nil {
		return tok, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)

	path := filepath.Join(dir, TokenFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// Another process created it first; use theirs.
		return ReadToken(dir)
	}
	if err != nil {
		return "", err
	}
	// Restrict access before the secret is written.
	if err := restrictToUser(path); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("restricting %s: %w", TokenFile, err)
	}
	_, werr := f.WriteString(tok + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(path)
		return "", err
	}
	return tok, nil
}
