package replay

import (
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
)

// TestRunCosts: usage is counted per stream and session (subagents apart
// and rolled up), a model without a price is listed and costs nothing,
// and each loop gets the cost of its span.
func TestRunCosts(t *testing.T) {
	root := writeSynthetic(t)
	streams, err := Discover(root, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// The synthetic transcripts use "claude-test"; price it.
	p := cost.DefaultPricing()
	p.Models = append(p.Models, cost.ModelPrice{Match: "claude-test", Rates: cost.Rates{Input: 1, Output: 10, CacheWrite: 1, CacheRead: 1}, Checked: "2026-10-21"})
	res := Run(streams, Options{Thresholds: config.Default(), Pricing: p})
	c := res.Costs
	if c == nil || len(c.Streams) != 2 || len(c.UnknownModels) != 0 {
		t.Fatalf("costs %+v", c)
	}
	// Every Tool call is one message: 3 input and 50 output tokens.
	calls := 0
	for _, s := range c.Streams {
		calls += s.Summary.Messages
	}
	if c.Usage.Input != int64(3*calls) || c.Usage.Output != int64(50*calls) {
		t.Errorf("usage %+v for %d messages", c.Usage, calls)
	}
	if want := float64(calls) * (3 + 500) / 1e6; c.CostUSD < want-1e-12 || c.CostUSD > want+1e-12 {
		t.Errorf("cost %v, want %v", c.CostUSD, want)
	}
	sess := c.SessionCosts()
	if len(sess) != 1 || sess[0].Streams != 2 || sess[0].CostUSD != c.CostUSD {
		t.Errorf("sessions %+v", sess)
	}
	var inLoops float64
	for _, l := range res.Loops {
		if l.CostUSD == nil || *l.CostUSD <= 0 {
			t.Errorf("loop %s without a cost", l.LoopKey)
		}
	}
	for _, s := range c.Streams {
		inLoops += s.WastedUSD
	}
	if inLoops <= 0 || inLoops > c.CostUSD || c.WastedCostUSD != inLoops {
		t.Errorf("cost inside loops %v of %v", c.WastedCostUSD, c.CostUSD)
	}

	// Without a price the model is listed, its tokens counted, cost 0.
	res = Run(streams, Options{Thresholds: config.Default(), Pricing: cost.DefaultPricing()})
	if c := res.Costs; len(c.UnknownModels) != 1 || c.UnknownModels[0] != "claude-test" || c.CostUSD != 0 || c.Unpriced.Output == 0 {
		t.Errorf("unpriced: %+v", c)
	}
	var b strings.Builder
	if err := NewReport(res, root, time.Time{}, ReportOptions{Pricing: cost.DefaultPricing()}, time.Unix(0, 0), 0).WriteText(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"API-equivalent", "Cost inside loops", "Not priced", "claude-test", "Cost by session"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, b.String())
		}
	}
}
