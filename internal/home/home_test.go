package home

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDirOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "with space", "lg")
	t.Setenv(EnvHome, want)
	if got, err := Dir(); err != nil || got != want {
		t.Errorf("Dir() = %q, %v; want %q", got, err, want)
	}
	t.Setenv(EnvHome, "")
	got, err := Dir()
	if err != nil || filepath.Base(got) != ".loopguard" {
		t.Errorf("default Dir() = %q, %v", got, err)
	}
}

func TestEnsureToken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "space in path")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Errorf("token %q: want 64 hex chars", tok)
	}
	again, err := EnsureToken(dir)
	if err != nil || again != tok {
		t.Errorf("second EnsureToken = %q, %v; want the same token", again, err)
	}
	if read, err := ReadToken(dir); err != nil || read != tok {
		t.Errorf("ReadToken = %q, %v", read, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, TokenFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("token mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestReadTokenMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, TokenFile), []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(dir); err == nil {
		t.Error("malformed token accepted")
	}
	if _, err := EnsureToken(dir); err == nil {
		t.Error("EnsureToken must not silently replace a malformed token file")
	}
}

func TestDaemonInfo(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadDaemonInfo(dir); err == nil {
		t.Error("missing daemon.json read without error")
	}
	want := DaemonInfo{PID: 1234, Port: 47822, Version: "v1", StartedAt: time.Unix(1700000000, 0).UTC()}
	if err := WriteDaemonInfo(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDaemonInfo(dir)
	if err != nil || got != want {
		t.Errorf("ReadDaemonInfo = %+v, %v; want %+v", got, err, want)
	}
	if err := os.WriteFile(filepath.Join(dir, DaemonFile), []byte(`{"pid":1,"port":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDaemonInfo(dir); err == nil {
		t.Error("port 0 accepted")
	}
}
