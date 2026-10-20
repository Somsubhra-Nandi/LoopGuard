package normalize

import (
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// Builder turns events into ToolCalls. It is safe for concurrent use.
type Builder struct {
	// Poll flags polling commands.
	Poll *CommandMatcher
	// ReadOnly matches read/search commands (grep, cat, git log, ...): their
	// output quotes other text, so failure markers in it are not a failure.
	ReadOnly *CommandMatcher
	// ErrorMarker matches the test and build runner families for which an
	// "Error:" line is a soft failure.
	ErrorMarker *FamilyMatcher
}

// NewBuilder returns a Builder using the given polling and read-only
// command matchers (either may be nil). The "Error:" soft-failure marker
// applies to no family; set ErrorMarker for that.
func NewBuilder(poll, readOnly *CommandMatcher) *Builder {
	return &Builder{Poll: poll, ReadOnly: readOnly}
}

// BuilderFor returns a Builder configured by th.
func BuilderFor(th config.Thresholds) *Builder {
	b := NewBuilder(
		NewCommandMatcher(th.PollingFamilies, th.PollingPatterns),
		NewCommandMatcher(th.ReadOnlyCommands, nil),
	)
	b.ErrorMarker = NewFamilyMatcher(th.ErrorMarkerFamilies)
	return b
}

// Build derives a ToolCall from a post (or post_failure) event. It reads
// only the event: FileHashAfter, Duration, and Progress are left for the
// caller and the session to fill in.
//
// A pre event can be passed too, to fingerprint an action before it runs;
// the output-derived fields are then empty.
func (b *Builder) Build(ev *event.Event) *event.ToolCall {
	in := parseInput(ev.Input)
	canon, cmd := canonicalInput(ev.ToolName, in, ev.Cwd)
	c := &event.ToolCall{
		Event:        ev,
		ActionFP:     actionFP(ev.ToolName, canon),
		Family:       ev.ToolName,
		FailingTests: -1,
		TodosDone:    -1,
	}
	readOnly := false
	if cmd != nil {
		c.Command = cmd.Canonical
		c.Family = cmd.Primary()
		c.Identity = Identity(cmd.PrimaryWords(), c.Family, ev.Cwd)
		c.Polling = b.Poll.Matches(*cmd)
		readOnly = b.ReadOnly.Matches(*cmd)
	}
	switch ev.ToolName {
	case event.ToolEdit:
		c.FilePath = NormalizePath(in.fileOf(), ev.Cwd)
		c.Edits = in.editPairs()
	case event.ToolWrite, event.ToolRead:
		c.FilePath = NormalizePath(in.fileOf(), ev.Cwd)
	case event.ToolPatch:
		c.FilePath = firstPatchFile(patchText(in), ev.Cwd)
	}
	if n, ok := todosDone(in); ok {
		c.TodosDone = n
	}

	if ev.Phase == event.PhasePre {
		return c
	}
	// Redact before normalizing: placeholders could otherwise split a token
	// and leave part of it behind (LLD §13).
	out := redact.String(tail(OutputText(ev.Output), MaxInput))
	norm := Text(out)
	c.ResultFP = HashString(norm)
	failed := ev.IsError || ev.Phase == event.PhasePostFailure
	if !failed && ev.ToolName == event.ToolBash && !readOnly && IsSoftFailure(out, b.ErrorMarker.Matches(c.Family)) {
		failed = true
	}
	if failed {
		c.ErrorSig = signatureOfNormalized(norm)
		c.FailingTests = FailingTests(norm)
	}
	return c
}

// tail keeps the last max bytes of an output, from a line start: errors
// are at the end of an output, and Text would otherwise keep the head.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return cutToLine(s[len(s)-max:])
}

func firstPatchFile(body, cwd string) string {
	for _, line := range strings.Split(body, "\n") {
		if p, ok := PatchFileHeader(strings.TrimRight(line, "\r")); ok {
			return NormalizePath(p, cwd)
		}
	}
	return ""
}

// todosDone counts completed items in a TodoWrite-style input, or reports
// a single task update to "completed".
func todosDone(in toolInput) (int, bool) {
	if len(in.Todos) > 0 {
		n := 0
		for _, t := range in.Todos {
			if t.Status == "completed" {
				n++
			}
		}
		return n, true
	}
	if in.Status == "completed" {
		return 1, true
	}
	return 0, false
}
