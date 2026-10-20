package event

import (
	"strings"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	a, b := NewID(t0), NewID(t0.Add(time.Millisecond))
	if len(a) != 26 || strings.Trim(a, crockford) != "" {
		t.Fatalf("NewID = %q: want 26 Crockford base32 characters", a)
	}
	if a >= b {
		t.Errorf("IDs don't sort by time: %q >= %q", a, b)
	}
	if x, y := NewID(t0), NewID(t0); x == y {
		t.Error("two IDs in the same millisecond are equal")
	}
	// The time prefix of a known instant (ULID spec: 10 characters of ms).
	if got := NewID(time.UnixMilli(0))[:10]; got != "0000000000" {
		t.Errorf("time prefix of the epoch = %q", got)
	}
}
