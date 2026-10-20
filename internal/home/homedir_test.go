package home

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/home/homedir"
)

// TestReadPortMatchesDaemonInfo checks the shim's JSON-free port reader
// against what the daemon writes.
func TestReadPortMatchesDaemonInfo(t *testing.T) {
	dir := t.TempDir()
	for _, port := range []int{1, 47821, 65535} {
		if err := WriteDaemonInfo(dir, DaemonInfo{PID: 99, Port: port, Version: `v"port": 1`, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if got, err := homedir.ReadPort(dir); err != nil || got != port {
			t.Errorf("ReadPort = %d, %v; want %d", got, err, port)
		}
	}
	for _, bad := range []string{``, `{}`, `{"port": 0}`, `{"port": 70000}`, `{"port": "x"}`, `{"port" 5}`, `not json`} {
		if err := os.WriteFile(filepath.Join(dir, DaemonFile), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := homedir.ReadPort(dir); err == nil {
			t.Errorf("ReadPort(%q) = %d, want an error", bad, got)
		}
	}
}
