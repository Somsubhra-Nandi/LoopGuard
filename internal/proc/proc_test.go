package proc

import (
	"os"
	"testing"
)

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(own pid) = false")
	}
	if Alive(0) || Alive(-1) {
		t.Error("Alive(<=0) = true")
	}
	// Pids are multiples of 4 on Windows and bounded on Unix; this one is
	// neither in use nor valid.
	if Alive(0x7ffffffd) {
		t.Error("Alive(unused pid) = true")
	}
}
