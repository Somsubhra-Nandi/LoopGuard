package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

func TestDoctorCostChecks(t *testing.T) {
	dir := isolate(t)
	db := filepath.Join(dir, home.DBFile)
	byName := func() map[string]adapter.Check {
		out := map[string]adapter.Check{}
		for _, c := range costChecks(dir, db) {
			out[c.Name] = c
		}
		return out
	}
	c := byName()
	if c["pricing"].Status != adapter.StatusWarn || !strings.Contains(c["pricing"].Detail, "no ") {
		t.Errorf("missing pricing.yaml: %+v", c["pricing"])
	}
	if c["budgets"].Detail != "No budget set. Example: loopguard budget set daily.soft_usd 20" {
		t.Errorf("budgets: %+v", c["budgets"])
	}
	if _, errOut, code := runCLI(t, "", "budget", "set", "daily.soft_usd", "20"); code != 0 {
		t.Fatal(errOut)
	}
	if c := byName(); !strings.Contains(c["budgets"].Detail, "daily cost usd soft $20.00") {
		t.Errorf("budgets after set: %+v", c["budgets"])
	}
	if _, errOut, code := runCLI(t, "", "budget", "clear", "daily.soft_usd"); code != 0 {
		t.Fatal(errOut)
	}

	if _, err := cost.EnsurePricing(dir); err != nil {
		t.Fatal(err)
	}
	c = byName()
	if c["pricing"].Status != adapter.StatusOK || !strings.Contains(c["pricing"].Detail, "checked 2026-10-21") {
		t.Errorf("shipped pricing: %+v", c["pricing"])
	}
	// The ids seen in transcripts are verified; the inferred ones are listed.
	if d := c["model ids"].Detail; !strings.Contains(d, "claude-sonnet-5-5") || !strings.Contains(d, "claude-fable-5-1") ||
		strings.Contains(d, "claude-opus-5-5") || strings.Contains(d, "claude-haiku-4-5,") || strings.Contains(d, "seen in the last") {
		t.Errorf("model ids: %+v", c["model ids"])
	}
	// An unverified price, and a model seen without any.
	p := string(cost.DefaultPricingYAML()) + "  - match: claude-new-9\n    input_per_mtok: 0\n    output_per_mtok: 0\n    cache_write_per_mtok: 0\n    cache_read_per_mtok: 0\n    checked: unverified\n"
	if err := os.WriteFile(filepath.Join(dir, cost.PricingFile), []byte(p), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	w := st.NewWriter(store.WriterOptions{})
	w.Enqueue(store.Usage{SessionKey: "claude-code:s", SessionID: "s", MessageID: "m", TS: time.Now(), Model: "mystery-model", Output: 1234})
	w.Enqueue(store.Usage{SessionKey: "claude-code:s", SessionID: "s", MessageID: "m2", TS: time.Now(), Model: "claude-sonnet-5-5", Output: 10})
	w.Close()
	st.Close()
	c = byName()
	if c["pricing"].Status != adapter.StatusWarn || !strings.Contains(c["pricing"].Detail, "claude-new-9") {
		t.Errorf("unverified: %+v", c["pricing"])
	}
	if u := c["unknown models"]; u.Status != adapter.StatusWarn || !strings.Contains(u.Detail, "mystery-model (1234 tokens)") {
		t.Errorf("unknown models: %+v", u)
	}
	if d := c["model ids"].Detail; !strings.Contains(d, "set id_verified: true for: claude-sonnet-5-5") {
		t.Errorf("an inferred id seen in usage: %s", d)
	}
}
