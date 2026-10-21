package pipeline

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

func readStep(path string, offset int) step {
	in := map[string]any{"file_path": path}
	if offset > 0 {
		in["offset"], in["limit"] = offset, 100
	}
	return step{Tool: event.ToolRead, Input: in, Text: "contents of " + path}
}

func failStep(i int) step {
	return step{Bash: fmt.Sprintf("probe%d --check", i), Result: "fail", Text: fmt.Sprintf("probe%d: error: not found", i)}
}

func run(t *testing.T, name string, steps []step) runResult {
	t.Helper()
	return runScenario(t, scenario{Name: name, Steps: steps}, config.Default())
}

func want(t *testing.T, r runResult, signals, decisions []string) {
	t.Helper()
	if !equalStrings(r.signals, signals) || !equalStrings(r.decisions, decisions) {
		t.Errorf("signals %q, want %q\ndecisions %q, want %q\n  %s", r.signals, signals, r.decisions, decisions, strings.Join(r.details, "\n  "))
	}
}

func TestReadThrash(t *testing.T) {
	t.Run("same file five times", func(t *testing.T) {
		var steps []step
		for i := 0; i < 6; i++ {
			steps = append(steps, readStep("src/cart.ts", 0))
		}
		// Fires at the 5th read, nudges once (cooldown), never escalates.
		r := run(t, "rt-file", steps)
		want(t, r, []string{"5:read_thrash", "6:read_thrash"}, []string{"5:nudge"})
	})
	t.Run("paging is not thrashing", func(t *testing.T) {
		var steps []step
		for i := 0; i < 8; i++ {
			steps = append(steps, readStep("src/big.ts", 1+100*i))
		}
		want(t, run(t, "rt-paging", steps), nil, nil)
	})
	t.Run("a write in between resets it", func(t *testing.T) {
		steps := []step{readStep("src/cart.ts", 0), readStep("src/cart.ts", 0), readStep("src/cart.ts", 0),
			{Edit: "src/cart.ts", Old: "a", New: "b"}, readStep("src/cart.ts", 0), readStep("src/cart.ts", 0), readStep("src/cart.ts", 0)}
		want(t, run(t, "rt-write", steps), nil, nil)
	})
	t.Run("progress suppresses it", func(t *testing.T) {
		var steps []step
		for i := 0; i < 6; i++ {
			// Each new file touched this turn scores 0.4.
			steps = append(steps, readStep("src/cart.ts", 0), step{Edit: fmt.Sprintf("src/f%d.ts", i), Old: "a", New: "b"})
		}
		want(t, run(t, "rt-progress", steps), nil, nil)
	})
	t.Run("the same search with the same result", func(t *testing.T) {
		var steps []step
		for i := 0; i < 4; i++ {
			steps = append(steps, step{Tool: event.ToolGrep, Input: map[string]any{"pattern": "applyDiscount", "path": "src"}, Text: "src/cart.ts:12"})
		}
		want(t, run(t, "rt-search", steps), []string{"4:read_thrash"}, []string{"4:nudge"})
	})
	t.Run("never denied or halted", func(t *testing.T) {
		var steps []step
		for i := 0; i < 40; i++ {
			steps = append(steps, readStep("src/cart.ts", 0))
		}
		r := run(t, "rt-long", steps)
		for _, d := range r.decisions {
			if !strings.HasSuffix(d, ":nudge") {
				t.Fatalf("read thrash escalated: %v", r.decisions)
			}
		}
	})
}

// stuck is a turn with no progress: two first reads, then failing
// commands (each new, so no family loop forms) with re-reads in between.
func stuck(n int) []step {
	steps := []step{readStep("src/a.ts", 0), readStep("src/b.ts", 0)}
	for i := 0; len(steps) < n; i++ {
		if i%8 == 3 {
			steps = append(steps, readStep([]string{"src/a.ts", "src/b.ts"}[i/8%2], 0))
			continue
		}
		steps = append(steps, failStep(i))
	}
	return steps
}

func TestStall(t *testing.T) {
	t.Run("no progress for 25 calls", func(t *testing.T) {
		// Two explorations (0.4) don't end a stall; it nudges once per turn
		// and signals again only 25 calls later.
		r := run(t, "st-stuck", stuck(55))
		want(t, r, []string{"25:stall", "50:stall"}, []string{"25:nudge"})
	})
	t.Run("a human prompt starts over", func(t *testing.T) {
		prompt := "try another way"
		steps := append(stuck(30), step{Prompt: &prompt})
		for i := 100; i < 125; i++ {
			steps = append(steps, failStep(i))
		}
		r := run(t, "st-turns", steps)
		want(t, r, []string{"25:stall", "55:stall"}, []string{"25:nudge", "55:nudge"})
	})
	t.Run("research is progress", func(t *testing.T) {
		// New files, URLs, and searches between the failures: what research
		// sessions look like (45 false stalls on real data without this).
		var steps []step
		for i := 0; i < 60; i++ {
			switch i % 4 {
			case 0:
				steps = append(steps, readStep(fmt.Sprintf("docs/page%d.md", i), 0))
			case 1:
				steps = append(steps, step{Tool: "other:WebFetch", Input: map[string]any{"url": fmt.Sprintf("https://example.com/%d", i), "prompt": "summarize"}, Text: "ok"})
			case 2:
				steps = append(steps, step{Tool: event.ToolGrep, Input: map[string]any{"pattern": fmt.Sprintf("term%d", i)}, Text: "none"})
			default:
				steps = append(steps, failStep(i))
			}
		}
		want(t, run(t, "st-research", steps), nil, nil)
	})
	t.Run("the same fetch again is not exploration", func(t *testing.T) {
		var steps []step
		for i := 0; i < 30; i++ {
			if i%2 == 0 {
				steps = append(steps, step{Tool: "other:WebFetch", Input: map[string]any{"url": "https://example.com/docs", "prompt": fmt.Sprint(i)}, Text: "ok"})
			} else {
				steps = append(steps, failStep(i))
			}
		}
		if r := run(t, "st-refetch", steps); len(r.signals) == 0 || r.signals[0] != "25:stall" {
			t.Errorf("signals %v, want a stall at call 25", r.signals)
		}
	})
	t.Run("quiet while a loop detector's loop is handled", func(t *testing.T) {
		// npm test failing the same way: D2 nudges at the 4th failure, and
		// the stall detector leaves that loop to it.
		var steps []step
		for i := 0; i < 30; i++ {
			steps = append(steps, step{Bash: "npm test", Result: "fail", Output: "jest/cart_total_fail_1.txt"})
		}
		r := run(t, "st-d2", steps)
		for _, s := range r.signals {
			if strings.HasSuffix(s, ":stall") {
				t.Fatalf("stall fired during a D2 loop: %v", r.signals)
			}
		}
	})
}

func TestStallMessage(t *testing.T) {
	sc := scenario{Name: "st-msg", Steps: stuck(25)}
	r := runScenario(t, sc, config.Default())
	if len(r.messages) == 0 {
		t.Fatal("no nudge")
	}
	m := r.messages[len(r.messages)-1]
	for _, w := range []string{"the last 25 tool calls made no visible progress", "20 calls failed and 3 calls repeated earlier reads"} {
		if !strings.Contains(m, w) {
			t.Errorf("message lacks %q: %s", w, m)
		}
	}
}
