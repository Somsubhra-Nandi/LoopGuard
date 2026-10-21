package cost

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9*math.Max(1, math.Abs(b)) }

func TestDefaultPricingIsVerified(t *testing.T) {
	p := DefaultPricing()
	if len(p.Models) == 0 {
		t.Fatal("no models")
	}
	if u := p.Unverified(); len(u) != 0 {
		t.Errorf("shipped prices without a check date: %v", u)
	}
	for _, m := range p.Models {
		if m.Source == "" || m.Input <= 0 || m.Output <= 0 || m.CacheRead <= 0 || m.CacheWrite <= 0 {
			t.Errorf("%s: incomplete entry %+v", m.Match, m)
		}
	}
	// Every model id seen in real transcripts is priced, except the
	// client's own synthetic messages.
	for _, id := range []string{"claude-opus-5-5", "claude-opus-5", "claude-opus-4-6", "claude-haiku-5-5", "claude-haiku-4-5-20251001",
		"claude-fable-5-1", "claude-sonnet-5-5", "claude-opus-4-6[1m]", "claude-3-5-haiku-20241022"} {
		if !p.Priced(id) {
			t.Errorf("%s has no price", id)
		}
	}
	if m, _ := p.Lookup("claude-opus-5"); m.Match != "claude-opus-5" {
		t.Errorf("claude-opus-5 matched %q", m.Match)
	}
	if m, _ := p.Lookup("claude-opus-5-5"); m.Match != "claude-opus-5-5" {
		t.Errorf("claude-opus-5-5 matched %q", m.Match)
	}
	if p.Priced("claude-opus-9") || p.Priced("gpt-5") {
		t.Error("an unknown model got a price")
	}
}

// TestCostMatchesClaudeCode prices the totals of a real session the way
// Claude Code's own cost-state line reported them (Opus 5.5, all cache
// writes to the one-hour cache): $10.6091082.
func TestCostMatchesClaudeCode(t *testing.T) {
	m := Message{Model: "claude-opus-5-5", Usage: Usage{Input: 3788, Output: 115029, CacheWrite: 520397, CacheWrite1h: 520397, CacheRead: 20651001}}
	got, ok := DefaultPricing().Cost(m)
	if !ok || !near(got, 10.6091082) {
		t.Errorf("cost %v %v, want 10.6091082", got, ok)
	}
}

func TestCostAllTokenTypes(t *testing.T) {
	p, err := ParsePricing([]byte(`
models:
  - match: m-*
    input_per_mtok: 1
    output_per_mtok: 10
    cache_write_per_mtok: 2
    cache_write_1h_per_mtok: 4
    cache_read_per_mtok: 0.5
    fast_multiplier: 3
    us_multiplier: 1.1
    long_prompt: { above_tokens: 1000, input_per_mtok: 100, output_per_mtok: 100, cache_write_per_mtok: 100, cache_read_per_mtok: 100 }
    checked: "2026-10-21"
  - match: n
    input_per_mtok: 1
    output_per_mtok: 1
    cache_write_per_mtok: 1
    cache_read_per_mtok: 1
    checked: unverified
`))
	if err != nil {
		t.Fatal(err)
	}
	u := Usage{Input: 100, Output: 10, CacheWrite: 300, CacheWrite1h: 100, CacheRead: 400}
	base := (100*1 + 10*10 + 200*2 + 100*4 + 400*0.5) / 1e6
	cases := []struct {
		name string
		m    Message
		want float64
		ok   bool
	}{
		{"each type at its rate", Message{Model: "m-1", Usage: u}, base, true},
		{"date suffix", Message{Model: "m-1-20260101", Usage: u}, base, true},
		{"fast", Message{Model: "m-1", Usage: u, Speed: "fast"}, 3 * base, true},
		{"us", Message{Model: "m-1", Usage: u, Geo: "us"}, 1.1 * base, true},
		{"fast and us stack", Message{Model: "m-1", Usage: u, Speed: "fast", Geo: "us"}, 3.3 * base, true},
		{"long prompt", Message{Model: "m-1", Usage: Usage{Input: 1001, Output: 1}}, 1002 * 100 / 1e6, true},
		{"1h rate unset uses the 5m rate", Message{Model: "m-1", Usage: Usage{CacheWrite: 10, CacheWrite1h: 10}}, 40 / 1e6, true},
		{"unverified", Message{Model: "n", Usage: u}, 0, false},
		{"unknown", Message{Model: "x", Usage: u}, 0, false},
		{"synthetic", Message{Model: SyntheticModel}, 0, true},
	}
	for _, c := range cases {
		got, ok := p.Cost(c.m)
		if ok != c.ok || !near(got, c.want) {
			t.Errorf("%s: %v %v, want %v %v", c.name, got, ok, c.want, c.ok)
		}
	}
	if len(p.Unverified()) != 1 {
		t.Errorf("unverified: %v", p.Unverified())
	}
}

func TestParsePricingRejects(t *testing.T) {
	for _, bad := range []string{
		"currency: EUR",
		"fx_rate: -1",
		"models: [{match: ''}]",
		"models: [{match: 'a[', checked: x}]",
		"models: [{match: a, input_per_mtok: -1}]",
		"models: [{match: a, input_pertok: 1}]", // typo: unknown key
	} {
		if _, err := ParsePricing([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestFormat(t *testing.T) {
	p := &Pricing{CurrencyDisplay: "INR", FXRate: 84}
	if got := p.Format(1.5); got != "$1.50 (INR 126.00)" {
		t.Errorf("format %q", got)
	}
	if got := (&Pricing{}).Format(0.004); got != "$0.0040" {
		t.Errorf("format %q", got)
	}
}

func TestLoadAndEnsurePricing(t *testing.T) {
	dir := t.TempDir()
	p, exists, err := LoadPricing(dir)
	if err != nil || exists || len(p.Models) == 0 {
		t.Fatalf("missing file: %v %v %v", exists, err, p)
	}
	if wrote, err := EnsurePricing(dir); !wrote || err != nil {
		t.Fatalf("ensure: %v %v", wrote, err)
	}
	path := filepath.Join(dir, PricingFile)
	if err := os.WriteFile(path, []byte("models: [{match: x, input_per_mtok: 1, checked: '2026-10-21'}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, _ := EnsurePricing(dir); wrote {
		t.Error("EnsurePricing overwrote a file")
	}
	if p, _, err := LoadPricing(dir); err != nil || len(p.Models) != 1 {
		t.Errorf("edited file: %v %v", err, p)
	}
	os.WriteFile(path, []byte("models: [{match: x, nope: 1}]"), 0o644)
	if p, _, err := LoadPricing(dir); err == nil || len(p.Models) < 2 {
		t.Errorf("invalid file: want the defaults and an error, got %v", err)
	}
}

func assistantLine(id, model string, u Usage, blocks string) string {
	if blocks == "" {
		blocks = `{"type":"text","text":"x"}`
	}
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-21T10:00:00.000Z","message":{"id":%q,"model":%q,"role":"assistant","content":[%s],`+
		`"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,`+
		`"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d},"speed":"standard","inference_geo":"global"}}}`,
		id, model, blocks, u.Input, u.Output, u.CacheWrite, u.CacheRead, u.CacheWrite-u.CacheWrite1h, u.CacheWrite1h)
}

func useBlock(id string) string {
	return fmt.Sprintf(`{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"x"}}`, id)
}

func resultLine(id string, chars int) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q,"is_error":false}]}}`,
		id, strings.Repeat("a", chars))
}

func TestParseLineAndLedger(t *testing.T) {
	if _, ok := ParseLine([]byte(`{"type":"user","message":{"content":"assistant"}}`)); ok {
		t.Error("a user line parsed as usage")
	}
	l := NewLedger()
	u1 := Usage{Input: 3, Output: 10, CacheWrite: 100, CacheWrite1h: 100, CacheRead: 1000}
	u2 := u1
	u2.Output = 50 // the same message, a later line: output grew
	for _, line := range []string{
		assistantLine("msg_1", "claude-opus-5-5", u1, `{"type":"thinking","thinking":""}`),
		assistantLine("msg_1", "claude-opus-5-5", u2, useBlock("t1")),
		assistantLine("msg_1", "claude-opus-5-5", u1, useBlock("t2")), // a stale reading never lowers a counter
		assistantLine("msg_2", "claude-opus-5-5", Usage{Input: 1, Output: 1}, ""),
	} {
		m, ok := ParseLine([]byte(line))
		if !ok {
			t.Fatalf("not parsed: %s", line)
		}
		l.Add(m)
	}
	if l.Len() != 2 {
		t.Fatalf("%d messages, want 2", l.Len())
	}
	want := Usage{Input: 4, Output: 51, CacheWrite: 100, CacheWrite1h: 100, CacheRead: 1000}
	if l.Total() != want {
		t.Errorf("total %+v, want %+v", l.Total(), want)
	}
	if l.Total().Total() != 4+51+100+1000 {
		t.Errorf("Total() %d", l.Total().Total())
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTailer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tl := &Tailer{Path: path}
	var got []string
	poll := func() bool {
		t.Helper()
		reset, err := tl.Poll(func(b []byte) { got = append(got, strings.TrimSpace(string(b))) })
		if err != nil {
			t.Fatal(err)
		}
		return reset
	}
	poll() // missing file: nothing, no error
	writeLines(t, path, "a\n", "b\n", "c-partial")
	poll()
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %v (a partial line must wait)", got)
	}
	writeLines(t, path, "-done\n")
	poll()
	if strings.Join(got, ",") != "a,b,c-partial-done" || tl.At.Offset != 19 {
		t.Fatalf("got %v at %d", got, tl.At.Offset)
	}

	// A new tailer from the stored offset reads only what is new.
	tl2 := &Tailer{Path: path, At: tl.At}
	writeLines(t, path, "d\n")
	var got2 []string
	tl2.Poll(func(b []byte) { got2 = append(got2, strings.TrimSpace(string(b))) })
	if strings.Join(got2, ",") != "d" {
		t.Fatalf("resumed: %v", got2)
	}

	// Truncated: start over.
	os.WriteFile(path, []byte("x\n"), 0o644)
	got = nil
	if !poll() || strings.Join(got, ",") != "x" {
		t.Fatalf("truncation: %v", got)
	}
	// Replaced by a different file at least as long: the fingerprint
	// differs, start over.
	os.WriteFile(path, []byte("y\nz\n"), 0o644)
	got = nil
	if !poll() || strings.Join(got, ",") != "y,z" {
		t.Fatalf("replacement: %v", got)
	}
	got = nil
	if poll() || len(got) != 0 {
		t.Fatalf("no change: %v", got)
	}
}

// TestTailerBigFile reads a large transcript (lines longer than the read
// buffer, megabytes in all) incrementally and counts its usage once.
func TestTailerBigFile(t *testing.T) {
	if testing.Short() {
		t.Skip("big file")
	}
	path := filepath.Join(t.TempDir(), "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const msgs = 400
	for i := 0; i < msgs; i++ {
		u := Usage{Input: 1, Output: 2, CacheRead: 3}
		id := fmt.Sprintf("msg_%d", i)
		// Each message on two lines (repeated usage), plus a 300 KB result.
		fmt.Fprintln(f, assistantLine(id, "claude-opus-5-5", u, useBlock("t"+id)))
		fmt.Fprintln(f, assistantLine(id, "claude-opus-5-5", u, ""))
		fmt.Fprintln(f, resultLine("t"+id, 300<<10))
	}
	f.Close()
	fi, _ := os.Stat(path)
	if fi.Size() < 100<<20 {
		t.Fatalf("file is only %d bytes", fi.Size())
	}
	tl := &Tailer{Path: path}
	l := NewLedger()
	lines := 0
	if _, err := tl.Poll(func(b []byte) {
		lines++
		if m, ok := ParseLine(b); ok {
			l.Add(m)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if lines != 3*msgs || l.Len() != msgs || l.Total() != (Usage{Input: msgs, Output: 2 * msgs, CacheRead: 3 * msgs}) {
		t.Errorf("lines %d, messages %d, total %+v", lines, l.Len(), l.Total())
	}
	if tl.At.Offset != fi.Size() {
		t.Errorf("offset %d, size %d", tl.At.Offset, fi.Size())
	}
}

// TestEstimatesOneLargeOutputDominates: a call whose 40,000-character
// result is carried by every later call dominates the estimates, and the
// estimates never add up to more than the stream's real cost.
func TestEstimatesOneLargeOutputDominates(t *testing.T) {
	s := NewScanner()
	feed := func(lines ...string) {
		for _, l := range lines {
			s.Line([]byte(l))
		}
	}
	cr := func(out int64) Usage { return Usage{Input: 5, Output: out, CacheRead: 20000} }
	s.MarkTurn()
	feed(assistantLine("m1", "claude-opus-5-5", cr(100), useBlock("small")+","+useBlock("big")),
		resultLine("small", 400), resultLine("big", 40000))
	for i := 2; i <= 6; i++ {
		id := fmt.Sprintf("m%d", i)
		feed(assistantLine(id, "claude-opus-5-5", cr(50), useBlock("t"+id)), resultLine("t"+id, 100))
	}
	s.Label("big", "bash: cat", 2)
	s.Label("small", "bash: ls", 1)
	sum := s.Summarize(DefaultPricing())
	if sum.Messages != 6 || len(sum.Calls) != 7 {
		t.Fatalf("messages %d, calls %d", sum.Messages, len(sum.Calls))
	}
	var big, small, total float64
	for _, c := range sum.Calls {
		total += c.EstUSD
		switch c.Group {
		case "bash: cat":
			big = c.EstUSD
			if c.ResultTokens != 10000 || c.LaterCalls != 5 || c.OutputTokens != 50 {
				t.Errorf("big call: %+v", c)
			}
		case "bash: ls":
			small = c.EstUSD
		}
	}
	// 10,000 tokens carried 5 times at the cache-read price, plus 50 output
	// tokens: 5e4 × 0.2/1e6 + 50 × 20/1e6.
	if !near(big, 0.011) {
		t.Errorf("big call est %v, want 0.011", big)
	}
	if big < 10*small {
		t.Errorf("big %v doesn't dominate small %v", big, small)
	}
	if total > sum.CostUSD+1e-12 {
		t.Errorf("estimates %v exceed the real cost %v", total, sum.CostUSD)
	}
}

func TestEstimatesCompactionAndCap(t *testing.T) {
	s := NewScanner()
	s.Line([]byte(assistantLine("m1", "claude-opus-5-5", Usage{Input: 1, Output: 1}, useBlock("a"))))
	s.Line([]byte(resultLine("a", 4_000_000)))
	s.Line([]byte(`{"type":"system","subtype":"compact_boundary","compactMetadata":{}}`))
	s.Line([]byte(assistantLine("m2", "claude-opus-5-5", Usage{Input: 1, Output: 1}, "")))
	sum := s.Summarize(DefaultPricing())
	if sum.Compactions != 1 || sum.Calls[0].LaterCalls != 0 {
		t.Errorf("a compaction must stop the carry: %+v", sum.Calls[0])
	}

	// Without the compaction the carried result would be worth far more
	// than the stream cost (input is tiny here): the estimate is capped.
	s = NewScanner()
	s.Line([]byte(assistantLine("m1", "claude-opus-5-5", Usage{Input: 1, Output: 1}, useBlock("a"))))
	s.Line([]byte(resultLine("a", 4_000_000)))
	s.Line([]byte(assistantLine("m2", "claude-opus-5-5", Usage{Input: 1, Output: 1}, "")))
	sum = s.Summarize(DefaultPricing())
	if !near(sum.Calls[0].EstUSD, sum.CostUSD) {
		t.Errorf("est %v, want it capped at the cost %v", sum.Calls[0].EstUSD, sum.CostUSD)
	}
}

func TestSummaryTurnsAndUnknownModels(t *testing.T) {
	s := NewScanner()
	s.Line([]byte(assistantLine("m0", "claude-opus-5-5", Usage{Output: 1000}, "")))
	s.MarkTurn()
	s.Line([]byte(assistantLine("m1", "claude-opus-5-5", Usage{Output: 1000}, "")))
	s.Line([]byte(assistantLine("m2", "mystery-model", Usage{Output: 7}, "")))
	sum := s.Summarize(DefaultPricing())
	if len(sum.Turns) != 2 || !near(sum.Turns[1].CostUSD, 0.02) || sum.Turns[1].Unpriced != 7 {
		t.Errorf("turns %+v", sum.Turns)
	}
	if sum.Unpriced.Output != 7 || !near(sum.CostUSD, 0.04) {
		t.Errorf("unpriced %+v, cost %v", sum.Unpriced, sum.CostUSD)
	}
	if !near(sum.CostBetween(1, 2), 0.02) {
		t.Errorf("cost between %v", sum.CostBetween(1, 2))
	}
}
