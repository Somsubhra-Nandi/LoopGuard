package normalize

import (
	"strings"
	"testing"
)

// TestBestLines checks the nudge evidence picked from each runner's real
// (or hand-written) output: the assertion payload or error message, never
// FAIL or summary lines.
func TestBestLines(t *testing.T) {
	for _, tt := range []struct {
		file    string
		want    []string // substrings, each in some picked line
		notWant []string // substrings no picked line may contain
	}{
		{"jest/cart_total_fail_1.txt", []string{"Expected: 7", "Received: 8"}, []string{"FAIL ./cart.test.js", "Tests:", "Test Suites:"}},
		{"jest/cart_tax_fail.txt", nil, []string{"FAIL", "Tests:"}},
		{"pytest/add_fail_1.txt", []string{"E assert 4 == 3"}, []string{"FAILURES", "1 failed", "test session starts"}},
		{"pytest/import_error.txt", []string{"KeyError: 'DATABASE_URL'"}, nil},
		{"gotest/add_fail_1.txt", []string{"calc_test.go:7: Add(1, 2) = 4, want 3"}, []string{"FAIL\t", "example.com/gocalc"}},
		{"cargo/add_fail_1.txt", []string{"left: 4", "right: 3"}, []string{"test result:", "error: test failed"}},
		{"cargo/compile_error.txt", []string{"error[E0308]: mismatched types"}, []string{"could not compile"}},
		{"tsc/type_fail_two.txt", []string{"error TS2322", "error TS2554"}, nil},
	} {
		sig := Signature(readOutput(t, tt.file))
		got := strings.Join(sig.Best, "\n")
		if len(sig.Best) == 0 || len(sig.Best) > 3 {
			t.Errorf("%s: %d best lines:\n%s", tt.file, len(sig.Best), got)
		}
		for _, w := range tt.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: best lines lack %q:\n%s", tt.file, w, got)
			}
		}
		for _, w := range tt.notWant {
			if strings.Contains(got, w) {
				t.Errorf("%s: best lines contain %q:\n%s", tt.file, w, got)
			}
		}
	}
}

func TestEvidenceScore(t *testing.T) {
	for line, want := range map[string]int{
		"Expected: 110":                  4,
		"Received: 100":                  4,
		"E assert 4 == 3":                4,
		"AssertionError: values differ":  4,
		"TypeError: x is not a function": 4,
		"src/a.ts(4,9): error TS2322: Type 'string' is not assignable": 4,
		"● cart › total › adds tax":                                    2,
		"--- FAIL: TestAdd (0.00s)":                                    2,
		"expect(received).toBe(expected) // Object.is equality":        3,
		"FAIL src/cart.test.ts":                                        0,
		"FAIL":                                                         0,
		"Tests: 1 failed, 7 passed, 8 total":                           0,
		"test result: FAILED. 2 passed; 1 failed":                      0,
		"some ordinary output":                                         0,
	} {
		if got := evidenceScore(line, IsKeyLine(line)); got != want {
			t.Errorf("evidenceScore(%q) = %d, want %d", line, got, want)
		}
	}
}
