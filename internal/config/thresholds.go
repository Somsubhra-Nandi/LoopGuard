package config

// Thresholds holds every tunable number the detectors and the policy use.
// Defaults follow LLD §6, §7, and §12; tune them with the replay harness.
type Thresholds struct {
	Detectors DetectorThresholds
	Policy    PolicyThresholds

	// PollingFamilies are command prefixes (non-flag words, e.g. "gh run
	// watch") whose repeats are expected and never count as loops.
	PollingFamilies []string
	// PollingPatterns are regular expressions matched against a normalized
	// command, for polling commands that depend on their arguments
	// (`kubectl get pods -w`, `curl …/health`).
	PollingPatterns []string

	// ReadOnlyCommands are command prefixes (like PollingFamilies) for
	// read/search commands. Their output quotes files and logs, so failure
	// markers in it ("Error:", "FAILED") never make the call a soft failure.
	ReadOnlyCommands []string

	// ErrorMarkerFamilies are command-family prefixes (test and build
	// runners) for which an "Error:" line counts as a soft failure. Other
	// programs print "Error:" lines as ordinary output.
	ErrorMarkerFamilies []string
}

// DetectorThresholds configures the detectors.
type DetectorThresholds struct {
	// Window is the maximum number of recent calls a detector looks at. The
	// window also never reaches back past the last human prompt.
	Window int
	// SimHashMaxDistance is the largest simhash Hamming distance at which two
	// error signatures still count as the same error.
	SimHashMaxDistance int

	ExactRepeat     ExactRepeat
	RepeatedFailure RepeatedFailure
	Oscillation     Oscillation
	ReadThrash      ReadThrash // TODO(M3): D4 is not implemented yet.
	Stall           Stall      // TODO(M3): D5 is not implemented yet.
}

// ExactRepeat configures D1.
type ExactRepeat struct {
	Enabled  bool
	MinCount int
}

// RepeatedFailure configures D2.
type RepeatedFailure struct {
	Enabled  bool
	MinCount int
	// MaxProgress: D2 only fires while the progress since the first failure
	// stays below this.
	MaxProgress float64
}

// Oscillation configures D3.
type Oscillation struct {
	Enabled bool
	// MinRevisits is the number of inverse edits, or of content-hash
	// revisits, needed to fire.
	MinRevisits int
	// RevisitWindow is how many recent writes to a file are searched for
	// revisits.
	RevisitWindow int
}

// ReadThrash configures D4.
type ReadThrash struct {
	Enabled  bool
	MinReads int
}

// Stall configures D5.
type Stall struct {
	Enabled bool
	Window  int
}

// PolicyThresholds configures the escalation state machine (LLD §7.1).
type PolicyThresholds struct {
	NudgeThreshold float64
	// DenyAfterNudgeOccurrences: loop occurrences after the nudge before
	// escalating to deny.
	DenyAfterNudgeOccurrences int
	// DenyMinSeverity: deny needs a signal at least this severe.
	DenyMinSeverity float64
	// HaltAfterDenyOccurrences: loop occurrences after the first deny before
	// halting.
	HaltAfterDenyOccurrences int
	// HaltAfterMinutes halts a denied loop that has lasted this long.
	HaltAfterMinutes float64
	// MaxNudgesPerTurn caps nudges across all loops in one human turn.
	MaxNudgesPerTurn int
	// NudgeCooldownCalls: at most one nudge per loop within this many calls.
	NudgeCooldownCalls int
	// ResetQuietCalls resets a nudged loop that has not signaled for this
	// many calls.
	ResetQuietCalls int
	// ResetProgress resets a nudged or denied loop once this much progress
	// has been made since it last escalated.
	ResetProgress float64
	// ManualModeExtraOccurrences raises the deny and halt occurrence
	// thresholds in permission mode "default", where a human approves
	// actions and is presumably watching.
	ManualModeExtraOccurrences int
}

// Default returns the default thresholds.
func Default() Thresholds {
	return Thresholds{
		Detectors: DetectorThresholds{
			Window:             50,
			SimHashMaxDistance: 4,
			ExactRepeat:        ExactRepeat{Enabled: true, MinCount: 3},
			RepeatedFailure:    RepeatedFailure{Enabled: true, MinCount: 4, MaxProgress: 1.0},
			Oscillation:        Oscillation{Enabled: true, MinRevisits: 2, RevisitWindow: 20},
			ReadThrash:         ReadThrash{Enabled: true, MinReads: 5},
			Stall:              Stall{Enabled: true, Window: 25},
		},
		Policy: PolicyThresholds{
			NudgeThreshold:             0.55,
			DenyAfterNudgeOccurrences:  3,
			DenyMinSeverity:            0.7,
			HaltAfterDenyOccurrences:   2,
			HaltAfterMinutes:           20,
			MaxNudgesPerTurn:           3,
			NudgeCooldownCalls:         10,
			ResetQuietCalls:            15,
			ResetProgress:              1.0,
			ManualModeExtraOccurrences: 1,
		},
		PollingFamilies: []string{
			"sleep", "watch", "gh run watch", "gh pr checks", "git status", "docker ps", "ls",
		},
		PollingPatterns: []string{
			`^kubectl\b.*\s(-w|--watch)\b`,
			`^curl\b.*/health`,
		},
		ReadOnlyCommands: []string{
			"grep", "rg", "ag", "cat", "head", "tail", "less", "more", "find", "fd", "ls", "tree", "wc",
			"git log", "git diff", "git show", "git status", "git blame", "git grep",
			"select-string", "get-content", "findstr", "type",
			"docker logs", "kubectl logs", "aws logs", "journalctl", "curl", "wget", "echo", "printf",
		},
		ErrorMarkerFamilies: []string{
			"pytest", "jest", "vitest", "go test", "cargo test", "cargo build", "tsc",
			"npm test", "yarn test", "pnpm test", "bun test",
			"npm run test", "yarn run test", "pnpm run test", "bun run test",
			"npx jest", "npx vitest", "npx tsc",
			"mvn", "mvnw", "gradle", "gradlew", "make", "dotnet test",
		},
	}
}
