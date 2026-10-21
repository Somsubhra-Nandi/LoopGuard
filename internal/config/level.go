package config

// levelRank orders the levels by how much LoopGuard does: off does
// nothing, observe records, notify tells the user, assist also nudges the
// agent, guard also denies and halts (LLD §12).
var levelRank = map[string]int{
	ModeOff:     0,
	ModeObserve: 1,
	ModeNotify:  2,
	ModeAssist:  3,
	ModeGuard:   4,
}

// Levels lists the levels a config file or `loopguard mode` may set, least
// protective first.
var Levels = []string{ModeObserve, ModeNotify, ModeAssist, ModeGuard}

// CanonicalMode maps a mode value to its level: "enforce" (the M3-A name)
// is guard; "" stays "" (unset, which means observe).
func CanonicalMode(m string) string {
	if m == ModeEnforce {
		return ModeGuard
	}
	return m
}

// LevelRank returns a level's rank (unset is observe; unknown is -1).
func LevelRank(m string) int {
	if m == "" {
		return levelRank[ModeObserve]
	}
	if r, ok := levelRank[CanonicalMode(m)]; ok {
		return r
	}
	return -1
}

// ValidMode reports whether m is a level a file may set.
func ValidMode(m string) bool {
	return m != ModeOff && m != "" && LevelRank(m) >= 0
}

// Notifies reports whether a level sends notifications.
func Notifies(level string) bool { return LevelRank(level) >= levelRank[ModeNotify] }

// modeText is a mode as the file wrote it, e.g. "mode: guard" or "mode:
// enforce (= guard)".
func modeText(written string) string {
	if written == ModeEnforce {
		return "mode: enforce (= guard)"
	}
	return "mode: " + written
}
