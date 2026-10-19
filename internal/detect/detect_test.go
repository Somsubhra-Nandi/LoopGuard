package detect

import (
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

func sig(det, key string, sev float64, seqs ...uint64) event.Signal {
	return event.Signal{Detector: det, LoopKey: key, Severity: sev, Evidence: event.Evidence{Seqs: seqs}}
}

func TestDedupD2WinsOverlappingD1(t *testing.T) {
	got := Dedup([]event.Signal{
		sig(event.DetectorExactRepeat, "er:1", 0.65, 1, 2, 3, 4),
		sig(event.DetectorRepeatedFailure, "rf:npm test:a", 0.55, 1, 2, 3, 4),
	})
	if len(got) != 1 || got[0].Detector != event.DetectorRepeatedFailure || got[0].Severity != 0.65 {
		t.Errorf("got %+v, want one D2 signal with severity 0.65", got)
	}

	// Tie: D2 wins too.
	got = Dedup([]event.Signal{
		sig(event.DetectorRepeatedFailure, "rf:x", 0.6, 3, 4),
		sig(event.DetectorExactRepeat, "er:1", 0.6, 4),
	})
	if len(got) != 1 || got[0].Detector != event.DetectorRepeatedFailure {
		t.Errorf("tie: got %+v", got)
	}
}

func TestDedupKeepsUnrelated(t *testing.T) {
	got := Dedup([]event.Signal{
		sig(event.DetectorExactRepeat, "er:1", 0.5, 7, 8, 9),
		sig(event.DetectorRepeatedFailure, "rf:x", 0.55, 1, 3, 5),
		sig(event.DetectorOscillation, "os:/a", 0.7, 2, 4),
	})
	if len(got) != 3 {
		t.Fatalf("got %d signals, want 3", len(got))
	}
	if got[0].LoopKey != "os:/a" || got[1].LoopKey != "rf:x" || got[2].LoopKey != "er:1" {
		t.Errorf("not sorted by severity: %v, %v, %v", got[0].LoopKey, got[1].LoopKey, got[2].LoopKey)
	}
}

func TestDedupSameKey(t *testing.T) {
	got := Dedup([]event.Signal{
		sig(event.DetectorOscillation, "os:/a", 0.6),
		sig(event.DetectorOscillation, "os:/a", 0.8),
	})
	if len(got) != 1 || got[0].Severity != 0.8 {
		t.Errorf("got %+v", got)
	}
}

func TestEngineRespectsEnabled(t *testing.T) {
	th := config.Default()
	if n := len(New(th).Detectors()); n != 3 {
		t.Errorf("default detectors = %d, want 3", n)
	}
	th.Detectors.ExactRepeat.Enabled = false
	th.Detectors.Oscillation.Enabled = false
	ds := New(th).Detectors()
	if len(ds) != 1 || ds[0].Name() != event.DetectorRepeatedFailure {
		t.Errorf("detectors = %v", ds)
	}
}
