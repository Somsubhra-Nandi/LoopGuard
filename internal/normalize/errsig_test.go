package normalize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
)

// Fixtures live in testdata/outputs/<toolchain>/. gotest, tsc, and jest are
// real outputs captured from tiny failing projects; pytest and cargo are
// hand-written (see testdata/outputs/README.md).
func readOutput(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "outputs", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSameErrorPairs(t *testing.T) {
	maxDist := config.Default().Detectors.SimHashMaxDistance
	tests := []struct {
		a, b string
		same bool
		why  string
	}{
		// go test (real)
		{"gotest/add_fail_1.txt", "gotest/add_fail_2.txt", true, "rerun: package duration differs"},
		{"gotest/add_fail_1.txt", "gotest/add_fail_shifted.txt", true, "edit above the test shifted the line number"},
		{"gotest/add_fail_1.txt", "gotest/add_fail_other_assert.txt", false, "different assertion in the same test"},
		{"gotest/add_fail_1.txt", "gotest/sub_fail.txt", false, "different test"},
		{"gotest/add_fail_1.txt", "gotest/build_fail.txt", false, "compile error vs test failure"},

		// tsc (real)
		{"tsc/type_fail_1.txt", "tsc/type_fail_2.txt", true, "rerun"},
		{"tsc/type_fail_1.txt", "tsc/type_fail_shifted.txt", true, "line shifted by an edit"},
		{"tsc/type_fail_1.txt", "tsc/type_fail_other.txt", false, "different error code and message"},
		{"tsc/type_fail_1.txt", "tsc/type_fail_two.txt", false, "a second error appeared"},
		// Known trade-off of masking locations: the same message at another
		// position counts as the same error.
		{"tsc/type_fail_1.txt", "tsc/type_fail_moved_same_msg.txt", true, "same message, other location"},

		// jest (real)
		{"jest/cart_total_fail_1.txt", "jest/cart_total_fail_2.txt", true, "rerun: time and 'estimated' differ"},
		{"jest/cart_total_fail_1.txt", "jest/cart_total_fail_shifted.txt", true, "code frame and stack lines shifted; PASS line order differs"},
		{"jest/cart_total_fail_1.txt", "jest/cart_total_fail_other_value.txt", false, "same test, different received value"},
		{"jest/cart_total_fail_1.txt", "jest/cart_tax_fail.txt", false, "different test"},

		// pytest (hand-written)
		{"pytest/add_fail_1.txt", "pytest/add_fail_2.txt", true, "rerun: duration and tmp_path differ"},
		{"pytest/add_fail_1.txt", "pytest/add_fail_shifted.txt", true, "line shifted by an edit"},
		{"pytest/add_fail_1.txt", "pytest/add_fail_other_assert.txt", false, "different assertion in the same test"},
		{"pytest/add_fail_1.txt", "pytest/sub_fail.txt", false, "different test"},
		{"pytest/add_fail_1.txt", "pytest/import_error.txt", false, "import error vs test failure"},

		// cargo test (hand-written)
		{"cargo/add_fail_1.txt", "cargo/add_fail_2.txt", true, "rerun without the Compiling line"},
		{"cargo/add_fail_1.txt", "cargo/add_fail_shifted.txt", true, "panic location shifted"},
		{"cargo/add_fail_1.txt", "cargo/add_fail_other_assert.txt", false, "different left/right values"},
		{"cargo/add_fail_1.txt", "cargo/sub_fail.txt", false, "different test"},
		{"cargo/add_fail_1.txt", "cargo/compile_error.txt", false, "compile error vs test failure"},
	}
	for _, tt := range tests {
		t.Run(tt.a+"~"+tt.b, func(t *testing.T) {
			a, b := Signature(readOutput(t, tt.a)), Signature(readOutput(t, tt.b))
			if got := SameError(a, b, maxDist); got != tt.same {
				t.Errorf("SameError = %v, want %v (%s); exact equal=%v hamming=%d\n a: %q\n b: %q",
					got, tt.same, tt.why, a.Exact == b.Exact, Hamming(a.Sim, b.Sim), a.Lines, b.Lines)
			}
			// Margin check: a non-match must not be a near miss either.
			if !tt.same && Hamming(a.Sim, b.Sim) <= 2*maxDist {
				t.Errorf("hamming %d is within 2x the threshold for a pair that should differ (%s)", Hamming(a.Sim, b.Sim), tt.why)
			}
		})
	}
}

func TestSignatureKeepsFileName(t *testing.T) {
	maxDist := config.Default().Detectors.SimHashMaxDistance
	msg := ": error TS2322: Type 'string' is not assignable to type 'number'.\n"
	cases := []struct {
		a, b string
		same bool
	}{
		{"src/auth.ts(14,7)" + msg, "src/db.ts(14,7)" + msg, false},
		{"src/auth.ts(14,7)" + msg, "src/auth.ts(31,2)" + msg, true}, // moved within the file
		{"src/auth.ts:14:7 - error TS2322: x\n", "src/db.ts:14:7 - error TS2322: x\n", false},
		{"  File \"app/auth.py\", line 12, in login\nKeyError: 'user'\n", "  File \"app/db.py\", line 12, in login\nKeyError: 'user'\n", false},
		{"auth_test.go:7: got 4, want 3\n", "db_test.go:7: got 4, want 3\n", false},
	}
	for _, c := range cases {
		a, b := Signature(c.a), Signature(c.b)
		if got := SameError(a, b, maxDist); got != c.same {
			t.Errorf("SameError(%q, %q) = %v, want %v (exact equal %v, loc equal %v, hamming %d)",
				c.a, c.b, got, c.same, a.Exact == b.Exact, a.Loc == b.Loc, Hamming(a.Sim, b.Sim))
		}
	}
	// Even a simhash collision does not join errors in different files.
	a, b := Signature(cases[0].a), Signature(cases[0].b)
	b.Sim, b.Exact = a.Sim, a.Exact+1
	if SameError(a, b, maxDist) {
		t.Error("same simhash in different files matched")
	}
}

func TestSignatureNearDuplicate(t *testing.T) {
	// Many error lines with one volatile token that normalization does not
	// catch (a 3-hex-digit request id). Exact differs; simhash still matches.
	var a, b strings.Builder
	a.WriteString("error: request a1f rejected by upstream\n")
	b.WriteString("error: request c3e rejected by upstream\n")
	for _, m := range []string{"auth", "billing", "orders", "users", "search", "cart", "audit", "mail", "media", "admin", "geo", "jobs"} {
		line := "error TS2307: Cannot find module '@acme/" + m + "' or its corresponding type declarations.\n"
		a.WriteString(line)
		b.WriteString(line)
	}
	sa, sb := Signature(a.String()), Signature(b.String())
	if sa.Exact == sb.Exact {
		t.Fatal("test setup: expected different exact hashes")
	}
	if !SameError(sa, sb, config.Default().Detectors.SimHashMaxDistance) {
		t.Errorf("near-duplicate not matched: hamming %d", Hamming(sa.Sim, sb.Sim))
	}
}

func TestSignatureKeyLines(t *testing.T) {
	sig := Signature(readOutput(t, "gotest/add_fail_1.txt"))
	want := []string{"--- FAIL: TestAdd (<DUR>)", "calc_test.go:7: Add(1, 2) = 4, want 3", "FAIL", "FAIL example.com/gocalc <DUR>"}
	if strings.Join(sig.Lines, "|") != strings.Join(want, "|") {
		t.Errorf("Lines = %q, want %q (evidence keeps real line numbers)", sig.Lines, want)
	}

	// No error-looking line: the last five non-empty lines.
	sig = Signature("one\ntwo\nthree\nfour\nfive\nsix\n")
	if strings.Join(sig.Lines, ",") != "two,three,four,five,six" {
		t.Errorf("fallback Lines = %q", sig.Lines)
	}

	// Only the tail is looked at.
	long := "error: early failure\n" + strings.Repeat("noise line\n", TailSize/10) + "error: late failure\n"
	sig = Signature(long)
	for _, l := range sig.Lines {
		if strings.Contains(l, "early") {
			t.Errorf("line outside the %d-byte tail used: %q", TailSize, l)
		}
	}
}

func TestSoftFailure(t *testing.T) {
	for _, s := range []string{
		"FAILED tests/test_a.py::test_add", "ok\n  FAILED x", "Error: boom", "Traceback (most recent",
		"panic: x", "npm ERR! code 1", "error[E0308]", "  ✕ adds (3 ms)", "\tAssertionError", "a\r\nError: b",
	} {
		if !IsSoftFailure(s, true) {
			t.Errorf("IsSoftFailure(%q, true) = false", s)
		}
	}
	for _, s := range []string{
		"ok", "PASS", "0 errors", "all good",
		// Markers count only at the start of a line.
		"1 FAILED", "KeyError: 'x'", "raise ValueError: bad", "log: panic: recovered", "test result: FAILED.",
	} {
		if IsSoftFailure(s, true) {
			t.Errorf("IsSoftFailure(%q, true) = true", s)
		}
	}
	// "Error:" counts only for test and build runners.
	if IsSoftFailure("Error: not found", false) {
		t.Error(`"Error:" counted for a non-runner command`)
	}
	if !IsSoftFailure("Error: x\nTraceback (most recent call last):", false) {
		t.Error("other markers must still count for non-runner commands")
	}

	markers, line := SoftFailureMarkers("KeyError: 'k'\nError: e\n  FAILED t", true)
	if strings.Join(markers, ",") != "FAILED,Error:" || line != "Error: e" {
		t.Errorf("SoftFailureMarkers = %q, %q", markers, line)
	}
	markers, line = SoftFailureMarkers("Error: e\n  FAILED t", false)
	if strings.Join(markers, ",") != "FAILED" || line != "  FAILED t" {
		t.Errorf("SoftFailureMarkers without Error: = %q, %q", markers, line)
	}
}

func TestFailingTests(t *testing.T) {
	tests := []struct {
		file string
		want int
	}{
		{"jest/cart_total_fail_1.txt", 2},
		{"jest/cart_tax_fail.txt", 1},
		{"pytest/add_fail_1.txt", 1},
		{"cargo/add_fail_1.txt", 1},
		{"gotest/add_fail_1.txt", 1},
		{"tsc/type_fail_1.txt", -1},
	}
	for _, tt := range tests {
		if got := FailingTests(Text(readOutput(t, tt.file))); got != tt.want {
			t.Errorf("FailingTests(%s) = %d, want %d", tt.file, got, tt.want)
		}
	}
	if got := FailingTests("Found 3 errors in 2 files."); got != 3 {
		t.Errorf("tsc summary: %d", got)
	}
}

func TestSimHash(t *testing.T) {
	w := strings.Fields("the quick brown fox jumps over the lazy dog")
	if a, b := SimHash(w, 4), SimHash(append([]string(nil), w...), 4); a != b {
		t.Error("not deterministic")
	}
	if SimHash(nil, 4) != 0 {
		t.Error("empty input should hash to 0")
	}
	if SimHash([]string{"a", "b"}, 4) == 0 {
		t.Error("short input should still hash")
	}
	if Hamming(0b1011, 0b0001) != 2 {
		t.Error("Hamming")
	}
	if SameError(nil, Signature("x"), 4) {
		t.Error("nil signature matched")
	}
}
