package capture

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

// FlagPath is the flag file that turns capture on: <home>/capture.on.
func FlagPath(dir string) string { return filepath.Join(dir, home.CaptureFlag) }

// Dir is where captures go: <home>/capture.
func Dir(dir string) string { return filepath.Join(dir, home.CaptureDir) }

// SetFlag turns capture on (creates the flag file) or off (removes it).
func SetFlag(dir string, on bool) error {
	p := FlagPath(dir)
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("capture on since "+time.Now().Format(time.RFC3339)+"\n"), 0o600)
}

// FlagOn reports whether the flag file exists.
func FlagOn(dir string) bool {
	_, err := os.Stat(FlagPath(dir))
	return err == nil
}

// SwitchTTL is how long Switch trusts its last look at the flag file, so
// the daemon stats it at most every few seconds, not on every event.
const SwitchTTL = 2 * time.Second

// Switch tells the daemon whether to capture: always when forced (the
// LOOPGUARD_CAPTURE=1 environment variable), else while the flag file
// exists. Turning capture on or off needs no restart.
type Switch struct {
	forced bool
	dir    string
	now    func() time.Time

	mu      sync.Mutex
	checked time.Time
	on      bool
}

// NewSwitch returns the switch for the LoopGuard home dir.
func NewSwitch(dir string, forced bool) *Switch {
	return &Switch{forced: forced, dir: dir, now: time.Now}
}

// Enabled reports whether capture is on.
func (s *Switch) Enabled() bool {
	if s == nil {
		return false
	}
	if s.forced {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now := s.now(); s.checked.IsZero() || now.Sub(s.checked) >= SwitchTTL || now.Before(s.checked) {
		s.on = FlagOn(s.dir)
		s.checked = now
	}
	return s.on
}

// Forced reports whether capture is on through LOOPGUARD_CAPTURE=1.
func (s *Switch) Forced() bool { return s != nil && s.forced }
