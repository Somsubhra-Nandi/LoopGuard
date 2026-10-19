package normalize

import (
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// Builder turns events into ToolCalls. It is safe for concurrent use.
type Builder struct {
	Poll *PollMatcher
}

// NewBuilder returns a Builder that uses poll to flag polling commands.
func NewBuilder(poll *PollMatcher) *Builder { return &Builder{Poll: poll} }

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
	if cmd != nil {
		c.Command = cmd.Canonical
		c.Family = cmd.Primary()
		if b.Poll != nil {
			c.Polling = b.Poll.IsPolling(*cmd)
		}
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
	out := OutputText(ev.Output)
	norm := Text(out)
	c.ResultFP = HashString(norm)
	failed := ev.IsError || ev.Phase == event.PhasePostFailure
	if !failed && ev.ToolName == event.ToolBash && IsSoftFailure(out) {
		failed = true
	}
	if failed {
		c.ErrorSig = signatureOfNormalized(norm)
		c.FailingTests = FailingTests(norm)
	}
	return c
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
