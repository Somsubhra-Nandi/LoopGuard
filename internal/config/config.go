package config

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// Config is the configuration in effect: the defaults, overridden by
// config.yaml, then tightened by a repo's .loopguard.yaml (LLD §12).
type Config struct {
	// Mode is the level: "observe", "notify", "assist", "guard", or "" when
	// no file sets it (which means observe: LoopGuard never acts unless
	// told to). "enforce" in a file is read as guard.
	Mode string
	// ModeSource names the file that set Mode ("" for the default), and
	// ModeWritten is the value as written there (e.g. "enforce").
	ModeSource, ModeWritten string
	// ObserveUntil keeps a new install in observe mode until this time,
	// even with mode: enforce (TDD D-8). Zero: no grace period.
	ObserveUntil            time.Time
	ObserveDaysAfterInstall int
	// Enabled is false when a file turns LoopGuard off; DisabledBy names
	// the file.
	Enabled    bool
	DisabledBy string
	Thresholds Thresholds
	// Notify is global only (config.yaml).
	Notify Notify
}

// Defaults returns the configuration with no files.
func Defaults() Config {
	return Config{ObserveDaysAfterInstall: 3, Enabled: true, Thresholds: Default(), Notify: DefaultNotify()}
}

// Apply overrides c with every key set in f (the global config.yaml,
// named source in explanations).
func (c Config) Apply(f File, source string) Config {
	if f.Mode != nil {
		c.Mode, c.ModeSource, c.ModeWritten = CanonicalMode(*f.Mode), source, *f.Mode
	}
	c.Notify = c.Notify.apply(f.Notify)
	if f.ObserveUntil != nil {
		c.ObserveUntil = *f.ObserveUntil
	}
	if f.ObserveDaysAfterInstall != nil {
		c.ObserveDaysAfterInstall = *f.ObserveDaysAfterInstall
	}
	if f.Enabled != nil && !*f.Enabled {
		c.Enabled, c.DisabledBy = false, source
	}
	th := &c.Thresholds
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	setF := func(dst *float64, v *float64) {
		if v != nil {
			*dst = *v
		}
	}
	setB := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	if d := f.Detectors; d != nil {
		set(&th.Detectors.Window, d.Window)
		set(&th.Detectors.SimHashMaxDistance, d.SimHashMaxDistance)
		if x := d.ExactRepeat; x != nil {
			setB(&th.Detectors.ExactRepeat.Enabled, x.Enabled)
			set(&th.Detectors.ExactRepeat.MinCount, x.MinCount)
		}
		if x := d.RepeatedFailure; x != nil {
			setB(&th.Detectors.RepeatedFailure.Enabled, x.Enabled)
			set(&th.Detectors.RepeatedFailure.MinCount, x.MinCount)
			setF(&th.Detectors.RepeatedFailure.MaxProgress, x.MaxProgress)
		}
		if x := d.Oscillation; x != nil {
			setB(&th.Detectors.Oscillation.Enabled, x.Enabled)
			set(&th.Detectors.Oscillation.MinRevisits, x.MinRevisits)
			set(&th.Detectors.Oscillation.RevisitWindow, x.RevisitWindow)
		}
		if x := d.ReadThrash; x != nil {
			setB(&th.Detectors.ReadThrash.Enabled, x.Enabled)
			set(&th.Detectors.ReadThrash.MinReads, x.MinReads)
		}
		if x := d.Stall; x != nil {
			setB(&th.Detectors.Stall.Enabled, x.Enabled)
			set(&th.Detectors.Stall.Window, x.Window)
		}
	}
	if p := f.Policy; p != nil {
		pt := &th.Policy
		setF(&pt.NudgeThreshold, p.NudgeThreshold)
		set(&pt.DenyAfterNudgeOccurrences, p.DenyAfterNudgeOccurrences)
		setF(&pt.DenyMinSeverity, p.DenyMinSeverity)
		set(&pt.HaltAfterDenyOccurrences, p.HaltAfterDenyOccurrences)
		setF(&pt.HaltAfterMinutes, p.HaltAfterMinutes)
		set(&pt.MaxNudgesPerTurn, p.MaxNudgesPerTurn)
		set(&pt.NudgeCooldownCalls, p.NudgeCooldownCalls)
		set(&pt.ResetQuietCalls, p.ResetQuietCalls)
		setF(&pt.ResetProgress, p.ResetProgress)
		set(&pt.ManualModeExtraOccurrences, p.ManualModeExtraOccurrences)
	}
	setList := func(dst *[]string, v *[]string) {
		if v != nil {
			*dst = append([]string(nil), (*v)...)
		}
	}
	setList(&th.PollingFamilies, f.PollingFamilies)
	setList(&th.PollingPatterns, f.PollingPatterns)
	setList(&th.ReadOnlyCommands, f.ReadOnlyCommands)
	setList(&th.ErrorMarkerFamilies, f.ErrorMarkerFamilies)
	setList(&th.SlowFamilies, f.SlowFamilies)
	return c
}

// Tighten applies a repo's .loopguard.yaml (named source) to c. A repo
// file may make LoopGuard more protective for the repo, never less than
// the user's own config:
//
//   - mode raises the level (observe < notify < assist < guard); a lower
//     level is ignored, and so is enabled: false (pause the repo instead);
//   - a detector's enabled: false turns it off (true is ignored);
//   - the occurrence and severity thresholds below are used only when
//     lower than the global value.
//
// Everything else (windows, reset rules, command lists, observe_until,
// notify) is global only. Each ignored key is returned as a note.
func (c Config) Tighten(f File, source string) (Config, []string) {
	var notes []string
	ignore := func(key, why string) { notes = append(notes, fmt.Sprintf("%s: %s ignored: %s", source, key, why)) }
	const globalOnly = "only config.yaml can set it"

	if f.Enabled != nil && !*f.Enabled && c.Enabled {
		ignore("enabled: false", "a repo file can't lower the level (to pause LoopGuard here: loopguard pause --repo)")
	}
	if f.Mode != nil {
		switch global := c.Mode; {
		case LevelRank(*f.Mode) > LevelRank(global):
			c.Mode, c.ModeSource, c.ModeWritten = CanonicalMode(*f.Mode), source, *f.Mode
		case LevelRank(*f.Mode) < LevelRank(global):
			ignore(modeText(*f.Mode), "a repo file can't lower the level (config.yaml: "+CanonicalMode(global)+")")
		}
	}
	if f.ObserveUntil != nil {
		ignore("observe_until", globalOnly)
	}
	if f.ObserveDaysAfterInstall != nil {
		ignore("observe_days_after_install", globalOnly)
	}

	th := &c.Thresholds
	lower := func(key string, dst *int, v *int) {
		switch {
		case v == nil:
		case *v <= *dst:
			*dst = *v
		default:
			ignore(fmt.Sprintf("%s: %d", key, *v), fmt.Sprintf("a repo file can only lower it (config: %d)", *dst))
		}
	}
	lowerF := func(key string, dst *float64, v *float64) {
		switch {
		case v == nil:
		case *v <= *dst:
			*dst = *v
		default:
			ignore(fmt.Sprintf("%s: %g", key, *v), fmt.Sprintf("a repo file can only lower it (config: %g)", *dst))
		}
	}
	disable := func(key string, dst *bool, v *bool) {
		switch {
		case v == nil:
		case !*v:
			*dst = false
		case !*dst:
			ignore(key+".enabled: true", "a repo file can't turn on a detector config.yaml turns off")
		}
	}
	if d := f.Detectors; d != nil {
		if d.Window != nil {
			ignore("detectors.window", globalOnly)
		}
		if d.SimHashMaxDistance != nil {
			ignore("detectors.simhash_max_distance", globalOnly)
		}
		if x := d.ExactRepeat; x != nil {
			disable("detectors.exact_repeat", &th.Detectors.ExactRepeat.Enabled, x.Enabled)
			lower("detectors.exact_repeat.min_count", &th.Detectors.ExactRepeat.MinCount, x.MinCount)
		}
		if x := d.RepeatedFailure; x != nil {
			disable("detectors.repeated_failure", &th.Detectors.RepeatedFailure.Enabled, x.Enabled)
			lower("detectors.repeated_failure.min_count", &th.Detectors.RepeatedFailure.MinCount, x.MinCount)
			if x.MaxProgress != nil {
				ignore("detectors.repeated_failure.max_progress", globalOnly)
			}
		}
		if x := d.Oscillation; x != nil {
			disable("detectors.oscillation", &th.Detectors.Oscillation.Enabled, x.Enabled)
			lower("detectors.oscillation.min_revisits", &th.Detectors.Oscillation.MinRevisits, x.MinRevisits)
			if x.RevisitWindow != nil {
				ignore("detectors.oscillation.revisit_window", globalOnly)
			}
		}
		if x := d.ReadThrash; x != nil {
			disable("detectors.read_thrash", &th.Detectors.ReadThrash.Enabled, x.Enabled)
			lower("detectors.read_thrash.min_reads", &th.Detectors.ReadThrash.MinReads, x.MinReads)
		}
		if x := d.Stall; x != nil {
			disable("detectors.stall", &th.Detectors.Stall.Enabled, x.Enabled)
			if x.Window != nil {
				ignore("detectors.stall.window", globalOnly)
			}
		}
	}
	if p := f.Policy; p != nil {
		pt := &th.Policy
		lowerF("policy.nudge_threshold", &pt.NudgeThreshold, p.NudgeThreshold)
		lower("policy.deny_after_nudges_occurrences", &pt.DenyAfterNudgeOccurrences, p.DenyAfterNudgeOccurrences)
		lowerF("policy.deny_min_severity", &pt.DenyMinSeverity, p.DenyMinSeverity)
		lower("policy.halt_after_deny_occurrences", &pt.HaltAfterDenyOccurrences, p.HaltAfterDenyOccurrences)
		lowerF("policy.halt_after_minutes", &pt.HaltAfterMinutes, p.HaltAfterMinutes)
		lower("policy.max_nudges_per_turn", &pt.MaxNudgesPerTurn, p.MaxNudgesPerTurn)
		lower("policy.nudge_cooldown_calls", &pt.NudgeCooldownCalls, p.NudgeCooldownCalls)
		lower("policy.manual_mode_extra_occurrences", &pt.ManualModeExtraOccurrences, p.ManualModeExtraOccurrences)
		if p.ResetQuietCalls != nil {
			ignore("policy.reset_quiet_calls", globalOnly)
		}
		if p.ResetProgress != nil {
			ignore("policy.reset_progress", globalOnly)
		}
	}
	// The one list a repo may extend: a repo knows its own slow commands,
	// and slow families only lose the time-based halt (counts still halt).
	if f.SlowFamilies != nil {
		for _, fam := range *f.SlowFamilies {
			if !slices.Contains(th.SlowFamilies, fam) {
				// Clip: never write into the global config's backing array.
				th.SlowFamilies = append(slices.Clip(th.SlowFamilies), fam)
			}
		}
	}
	for key, set := range map[string]bool{
		"polling_families": f.PollingFamilies != nil, "polling_patterns": f.PollingPatterns != nil,
		"read_only_commands": f.ReadOnlyCommands != nil, "error_marker_families": f.ErrorMarkerFamilies != nil,
		"notify": f.Notify != nil, "privacy": f.Privacy != nil,
	} {
		if set {
			ignore(key, globalOnly)
		}
	}
	sort.Strings(notes)
	return c, notes
}

// ModeInfo is the level in effect and why, in words for doctor and status.
type ModeInfo struct {
	Mode string `json:"mode"` // off | observe | notify | assist | guard
	Why  string `json:"why"`
}

// ModeOff is the effective level when config.yaml turns LoopGuard off.
const ModeOff = "off"

// EffectiveMode resolves c's level at now: off when disabled; the level a
// file sets (the higher of config.yaml and the repo file) once any
// observe_until grace period has passed; observe otherwise.
func (c Config) EffectiveMode(now time.Time) ModeInfo {
	written := c.ModeWritten
	if written == "" {
		written = c.Mode
	}
	switch {
	case !c.Enabled:
		return ModeInfo{ModeOff, "enabled: false in " + c.DisabledBy}
	case c.Mode == "":
		return ModeInfo{ModeObserve, "the default: no mode set in " + GlobalFile}
	case c.Mode == ModeObserve:
		return ModeInfo{ModeObserve, modeText(written) + " in " + c.ModeSource}
	case now.Before(c.ObserveUntil):
		return ModeInfo{ModeObserve, "observe until " + c.ObserveUntil.Local().Format("2006-01-02 15:04") +
			" after a new install, then " + c.Mode}
	}
	return ModeInfo{c.Mode, modeText(written) + " in " + c.ModeSource}
}
