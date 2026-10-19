package pipeline

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
)

// driftingFailure returns the output of the i-th run of a failing build: the
// same dozen errors every time, plus a short request id that normalization
// does not catch. The exact signatures differ from run to run, while the
// simhashes stay within the same-error threshold.
func driftingFailure(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: request %03x rejected by upstream\n", 0xa00+i*7)
	for _, m := range []string{"auth", "billing", "orders", "users", "search", "cart", "audit", "mail", "media", "admin", "geo", "jobs"} {
		b.WriteString("error TS2307: Cannot find module '@acme/" + m + "' or its corresponding type declarations.\n")
	}
	return b.String()
}

// longLoop is 30 identical-error failures with an edit after each: 60 calls,
// longer than the 50-call detector window.
func longLoop() scenario {
	sc := scenario{Name: "long loop"}
	for i := 0; i < 30; i++ {
		sc.Steps = append(sc.Steps,
			step{Bash: "npm run build", Result: "fail", Text: driftingFailure(i)},
			step{Edit: "src/index.ts", Old: fmt.Sprintf("v%d", i), New: fmt.Sprintf("v%d", i+1)},
		)
	}
	return sc
}

func TestLongLoopKeepsOneLoopKey(t *testing.T) {
	maxDist := config.Default().Detectors.SimHashMaxDistance
	first := normalize.Signature(driftingFailure(0))
	for i := 1; i < 30; i++ {
		s := normalize.Signature(driftingFailure(i))
		if s.Exact == first.Exact {
			t.Fatalf("setup: run %d has the same exact signature as run 0", i)
		}
		if !normalize.SameError(first, s, maxDist) {
			t.Fatalf("setup: run %d is not the same error as run 0 (hamming %d)", i, normalize.Hamming(first.Sim, s.Sim))
		}
	}

	t.Run("default thresholds", func(t *testing.T) {
		res := runScenario(t, longLoop(), config.Default())
		assertOneKey(t, res)
		want := []string{"7:nudge", "15:deny", "17:deny", "19:halt"}
		if !equalStrings(res.decisions, want) {
			t.Errorf("decisions = %q, want %q", res.decisions, want)
		}
	})

	t.Run("no halt: escalation never restarts", func(t *testing.T) {
		th := config.Default()
		th.Policy.HaltAfterDenyOccurrences = 1000
		th.Policy.HaltAfterMinutes = 1e6
		res := runScenario(t, longLoop(), th)
		assertOneKey(t, res)
		nudges, denies := 0, 0
		for _, d := range res.decisions {
			switch {
			case strings.HasSuffix(d, ":nudge"):
				nudges++
			case strings.HasSuffix(d, ":deny"):
				denies++
			}
		}
		// Nudge at the 4th failure, deny armed at the 7th, then every later
		// attempt (8th..30th) would be denied.
		if nudges != 1 || denies != 23 {
			t.Errorf("nudges = %d, denies = %d, want 1 and 23; decisions %q", nudges, denies, res.decisions)
		}
		if last := res.signals[len(res.signals)-1]; last != "59:repeated_failure" {
			t.Errorf("last signal = %s, want the loop to still signal at call 59", last)
		}
	})
}

func assertOneKey(t *testing.T, res runResult) {
	t.Helper()
	if len(res.keys) == 0 {
		t.Fatal("no signals")
	}
	for i, k := range res.keys {
		if k != res.keys[0] {
			t.Fatalf("signal %s has key %s, want %s throughout", res.signals[i], k, res.keys[0])
		}
	}
}
