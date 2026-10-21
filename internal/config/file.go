package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// File names (LLD §12).
const (
	GlobalFile = "config.yaml"     // in the LoopGuard home
	RepoFile   = ".loopguard.yaml" // at a repo's git root
)

// Levels (the mode key), least to most protective after off (LLD §12).
const (
	ModeObserve = "observe" // record only
	ModeNotify  = "notify"  // notify the user; never touch the agent
	ModeAssist  = "assist"  // notify and nudge; never deny or halt
	ModeGuard   = "guard"   // nudge, deny, halt, and notify
	// ModeEnforce is the old name of guard (M3-A), still accepted in files
	// and by `loopguard mode`.
	ModeEnforce = "enforce"
)

// File is the YAML schema shared by config.yaml and .loopguard.yaml. Every
// field is a pointer, so a key that is absent can be told from one set to
// its zero value: absent keys keep the defaults (or, in a repo file, the
// global value).
type File struct {
	Mode                    *string    `yaml:"mode"`
	ObserveDaysAfterInstall *int       `yaml:"observe_days_after_install"`
	ObserveUntil            *time.Time `yaml:"observe_until"`
	// Enabled: false turns LoopGuard off (a kill switch). Meant for
	// .loopguard.yaml; in config.yaml it turns LoopGuard off everywhere.
	Enabled   *bool          `yaml:"enabled"`
	Detectors *DetectorsFile `yaml:"detectors"`
	Policy    *PolicyFile    `yaml:"policy"`

	PollingFamilies     *[]string `yaml:"polling_families"`
	PollingPatterns     *[]string `yaml:"polling_patterns"`
	ReadOnlyCommands    *[]string `yaml:"read_only_commands"`
	ErrorMarkerFamilies *[]string `yaml:"error_marker_families"`
	// SlowFamilies never halt on time. A repo file may add to the list.
	SlowFamilies *[]string `yaml:"slow_families"`

	Notify *NotifyFile `yaml:"notify"`

	Budgets *BudgetsFile `yaml:"budgets"`

	// Read in a later milestone (retention); accepted now so a config
	// written from LLD §12 is valid.
	Privacy map[string]any `yaml:"privacy"`
}

// DetectorsFile is the detectors: section.
type DetectorsFile struct {
	Window             *int `yaml:"window"`
	SimHashMaxDistance *int `yaml:"simhash_max_distance"`
	ExactRepeat        *struct {
		Enabled  *bool `yaml:"enabled"`
		MinCount *int  `yaml:"min_count"`
	} `yaml:"exact_repeat"`
	RepeatedFailure *struct {
		Enabled     *bool    `yaml:"enabled"`
		MinCount    *int     `yaml:"min_count"`
		MaxProgress *float64 `yaml:"max_progress"`
	} `yaml:"repeated_failure"`
	Oscillation *struct {
		Enabled       *bool `yaml:"enabled"`
		MinRevisits   *int  `yaml:"min_revisits"`
		RevisitWindow *int  `yaml:"revisit_window"`
	} `yaml:"oscillation"`
	ReadThrash *struct {
		Enabled  *bool `yaml:"enabled"`
		MinReads *int  `yaml:"min_reads"`
	} `yaml:"read_thrash"`
	Stall *struct {
		Enabled *bool `yaml:"enabled"`
		Window  *int  `yaml:"window"`
	} `yaml:"stall"`
}

// PolicyFile is the policy: section.
type PolicyFile struct {
	NudgeThreshold             *float64 `yaml:"nudge_threshold"`
	DenyAfterNudgeOccurrences  *int     `yaml:"deny_after_nudges_occurrences"`
	DenyMinSeverity            *float64 `yaml:"deny_min_severity"`
	HaltAfterDenyOccurrences   *int     `yaml:"halt_after_deny_occurrences"`
	HaltAfterMinutes           *float64 `yaml:"halt_after_minutes"`
	MaxNudgesPerTurn           *int     `yaml:"max_nudges_per_turn"`
	NudgeCooldownCalls         *int     `yaml:"nudge_cooldown_calls"`
	ResetQuietCalls            *int     `yaml:"reset_quiet_calls"`
	ResetProgress              *float64 `yaml:"reset_progress"`
	ManualModeExtraOccurrences *int     `yaml:"manual_mode_extra_occurrences"`
}

// Parse decodes a config file. An empty file is valid and sets nothing.
// Unknown keys don't make the file invalid (a newer LoopGuard may have
// written them); they are returned as warnings. Syntax errors, values of
// the wrong type, and values that fail Validate are errors.
func Parse(b []byte) (File, []string, error) {
	var f File
	var warnings []string
	strict := yaml.NewDecoder(bytes.NewReader(b))
	strict.KnownFields(true)
	err := strict.Decode(&f)
	if err != nil && !errors.Is(err, io.EOF) {
		// Retry without the unknown-key check: if that works, the only
		// problem was unknown keys.
		f = File{}
		if lerr := yaml.Unmarshal(b, &f); lerr != nil {
			return File{}, nil, lerr
		}
		warnings = append(warnings, err.Error())
	}
	if err := f.Validate(); err != nil {
		return File{}, nil, err
	}
	return f, warnings, nil
}

// Validate checks the values that are set.
func (f File) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if f.Mode != nil && !ValidMode(*f.Mode) {
		bad("mode: %q is not observe, notify, assist, or guard", *f.Mode)
	}
	f.Notify.validate(bad)
	f.Budgets.validate(bad)
	if v := f.ObserveDaysAfterInstall; v != nil && *v < 0 {
		bad("observe_days_after_install: %d is negative", *v)
	}
	atLeast := func(name string, v *int, lo int) {
		if v != nil && *v < lo {
			bad("%s: %d is below %d", name, *v, lo)
		}
	}
	fraction := func(name string, v *float64) {
		if v != nil && (*v <= 0 || *v > 1) {
			bad("%s: %g is not in (0, 1]", name, *v)
		}
	}
	if d := f.Detectors; d != nil {
		atLeast("detectors.window", d.Window, 1)
		atLeast("detectors.simhash_max_distance", d.SimHashMaxDistance, 0)
		if x := d.ExactRepeat; x != nil {
			atLeast("detectors.exact_repeat.min_count", x.MinCount, 2)
		}
		if x := d.RepeatedFailure; x != nil {
			atLeast("detectors.repeated_failure.min_count", x.MinCount, 2)
			if x.MaxProgress != nil && *x.MaxProgress <= 0 {
				bad("detectors.repeated_failure.max_progress: %g is not positive", *x.MaxProgress)
			}
		}
		if x := d.Oscillation; x != nil {
			atLeast("detectors.oscillation.min_revisits", x.MinRevisits, 1)
			atLeast("detectors.oscillation.revisit_window", x.RevisitWindow, 2)
		}
		if x := d.ReadThrash; x != nil {
			atLeast("detectors.read_thrash.min_reads", x.MinReads, 2)
		}
		if x := d.Stall; x != nil {
			atLeast("detectors.stall.window", x.Window, 2)
		}
	}
	if p := f.Policy; p != nil {
		fraction("policy.nudge_threshold", p.NudgeThreshold)
		fraction("policy.deny_min_severity", p.DenyMinSeverity)
		atLeast("policy.deny_after_nudges_occurrences", p.DenyAfterNudgeOccurrences, 0)
		atLeast("policy.halt_after_deny_occurrences", p.HaltAfterDenyOccurrences, 0)
		if p.HaltAfterMinutes != nil && *p.HaltAfterMinutes <= 0 {
			bad("policy.halt_after_minutes: %g is not positive", *p.HaltAfterMinutes)
		}
		atLeast("policy.max_nudges_per_turn", p.MaxNudgesPerTurn, 0)
		atLeast("policy.nudge_cooldown_calls", p.NudgeCooldownCalls, 0)
		atLeast("policy.reset_quiet_calls", p.ResetQuietCalls, 1)
		if p.ResetProgress != nil && *p.ResetProgress <= 0 {
			bad("policy.reset_progress: %g is not positive", *p.ResetProgress)
		}
		atLeast("policy.manual_mode_extra_occurrences", p.ManualModeExtraOccurrences, 0)
	}
	if f.PollingPatterns != nil {
		for _, p := range *f.PollingPatterns {
			if _, err := regexp.Compile(p); err != nil {
				bad("polling_patterns: %v", err)
			}
		}
	}
	return errors.Join(errs...)
}
