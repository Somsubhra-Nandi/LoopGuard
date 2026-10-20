package config

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultReloadEvery is how long a loaded file is trusted before its
// modification time is checked again.
const DefaultReloadEvery = 2 * time.Second

// maxFileSize caps config files; anything larger is refused as invalid.
const maxFileSize = 1 << 20

// LoaderOptions configures a Loader.
type LoaderOptions struct {
	// ReloadEvery is the stat interval (0: DefaultReloadEvery; negative:
	// stat on every call).
	ReloadEvery time.Duration
	// Thresholds, if set, replaces the thresholds from config.yaml (tests).
	Thresholds *Thresholds
	// OnIssue is called when a file is (re)loaded and is invalid (err) or
	// has unknown keys or ignored repo keys (warnings). It runs with the
	// loader's lock held, so it must not call back into the loader.
	OnIssue func(path string, err error, warnings []string)
	Now     func() time.Time
}

// Loader reads config.yaml and .loopguard.yaml files and caches them,
// re-reading a file when its modification time or size changes (checked
// at most every ReloadEvery). It is safe for concurrent use.
type Loader struct {
	home string
	o    LoaderOptions

	mu     sync.Mutex
	global *fileCache
	repos  map[string]*fileCache
	eff    map[string]effCache // repo root → last result
}

type fileCache struct {
	path     string
	checked  time.Time
	exists   bool
	modTime  time.Time
	size     int64
	file     File
	err      error
	warnings []string
	version  uint64
}

type effCache struct {
	gv, rv uint64
	eff    Effective
}

// Loaded describes one config file.
type Loaded struct {
	Path     string   `json:"path"`
	Exists   bool     `json:"exists"`
	Err      error    `json:"-"`
	Warnings []string `json:"warnings,omitempty"`
}

// Effective is the configuration for sessions in one repo.
type Effective struct {
	Config
	Global Loaded
	Repo   Loaded // Path is "" outside a repo
	// Notes lists repo keys that were ignored because they would loosen
	// the global config.
	Notes []string
	// Key identifies Thresholds, so callers can cache what they build from
	// them (detectors, pipelines).
	Key uint64
}

// ModeAt is EffectiveMode, explaining a fallback to the defaults.
func (e Effective) ModeAt(now time.Time) ModeInfo {
	m := e.EffectiveMode(now)
	if e.Global.Err != nil && e.ModeSource == "" && m.Mode == ModeObserve {
		m.Why = "the default: " + GlobalFile + " is invalid, so the defaults are used (see loopguard doctor)"
	}
	return m
}

// NewLoader returns a loader for the config.yaml in home.
func NewLoader(home string, o LoaderOptions) *Loader {
	if o.ReloadEvery == 0 {
		o.ReloadEvery = DefaultReloadEvery
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Loader{
		home:   home,
		o:      o,
		global: &fileCache{path: filepath.Join(home, GlobalFile)},
		repos:  map[string]*fileCache{},
		eff:    map[string]effCache{},
	}
}

// GlobalPath is the path of config.yaml.
func (l *Loader) GlobalPath() string { return l.global.path }

// For returns the configuration for a repo ("" for none): the defaults,
// config.yaml, then the repo's .loopguard.yaml. An invalid file is
// skipped (its error is in Global.Err or Repo.Err), so a typo never stops
// LoopGuard; it falls back to the defaults, which observe.
func (l *Loader) For(repoRoot string) Effective {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.o.Now()
	l.refresh(l.global, now)
	var rc *fileCache
	if repoRoot != "" {
		rc = l.repos[repoRoot]
		if rc == nil {
			rc = &fileCache{path: filepath.Join(repoRoot, RepoFile)}
			l.repos[repoRoot] = rc
		}
		l.refresh(rc, now)
	}
	gv, rv := l.global.version, uint64(0)
	if rc != nil {
		rv = rc.version
	}
	if c, ok := l.eff[repoRoot]; ok && c.gv == gv && c.rv == rv {
		return c.eff
	}

	e := Effective{Config: Defaults(), Global: l.global.loaded()}
	if l.global.exists && l.global.err == nil {
		e.Config = e.Apply(l.global.file, GlobalFile)
	}
	if l.o.Thresholds != nil {
		e.Thresholds = *l.o.Thresholds
	}
	if rc != nil {
		e.Repo = rc.loaded()
		if rc.exists && rc.err == nil {
			e.Config, e.Notes = e.Tighten(rc.file, rc.path)
		}
	}
	e.Key = thresholdsKey(e.Thresholds)
	l.eff[repoRoot] = effCache{gv: gv, rv: rv, eff: e}
	return e
}

func (fc *fileCache) loaded() Loaded {
	return Loaded{Path: fc.path, Exists: fc.exists, Err: fc.err, Warnings: fc.warnings}
}

// refresh re-reads fc if it changed on disk, at most every ReloadEvery.
func (l *Loader) refresh(fc *fileCache, now time.Time) {
	if fc.version > 0 && l.o.ReloadEvery > 0 && now.Sub(fc.checked) < l.o.ReloadEvery {
		return
	}
	fc.checked = now
	fi, err := os.Stat(fc.path)
	exists := err == nil
	if fc.version > 0 && exists == fc.exists && (!exists || (fi.ModTime().Equal(fc.modTime) && fi.Size() == fc.size)) {
		return
	}
	fc.version++
	fc.exists, fc.file, fc.err, fc.warnings = exists, File{}, nil, nil
	fc.modTime, fc.size = time.Time{}, 0
	if !exists {
		if !errors.Is(err, fs.ErrNotExist) {
			fc.exists, fc.err = true, err // e.g. permission denied
		}
	} else {
		fc.modTime, fc.size = fi.ModTime(), fi.Size()
		fc.file, fc.warnings, fc.err = readFile(fc.path, fi)
	}
	if l.o.OnIssue != nil && (fc.err != nil || len(fc.warnings) > 0) {
		l.o.OnIssue(fc.path, fc.err, fc.warnings)
	}
}

func readFile(path string, fi fs.FileInfo) (File, []string, error) {
	if fi.IsDir() {
		return File{}, nil, errors.New("is a directory")
	}
	if fi.Size() > maxFileSize {
		return File{}, nil, fmt.Errorf("larger than %d bytes", maxFileSize)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, nil, err
	}
	return Parse(b)
}

// LoadFile reads and parses one config file; a missing file is an empty
// one (exists false).
func LoadFile(path string) (f File, exists bool, warnings []string, err error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, nil, nil
	}
	if err != nil {
		return File{}, true, nil, err
	}
	f, warnings, err = readFile(path, fi)
	return f, true, warnings, err
}

// thresholdsKey hashes the thresholds' printed form, which covers every
// field including the command lists.
func thresholdsKey(th Thresholds) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%#v", th)
	return h.Sum64()
}
