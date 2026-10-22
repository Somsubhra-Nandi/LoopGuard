package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
)

func TestBudgetCommand(t *testing.T) {
	dir := isolate(t)
	path := filepath.Join(dir, config.GlobalFile)

	out, _, code := runCLI(t, "", "budget")
	if code != 0 || !strings.Contains(out, "No budget set. Example: loopguard budget set daily.soft_usd 20") {
		t.Fatalf("show with no file: %d %s", code, out)
	}
	if err := os.WriteFile(path, []byte("# my settings\nmode: guard # keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, errOut, code := runCLI(t, "", "budget", "set", "daily.soft_usd", "$20")
	if code != 0 || !strings.Contains(out, "Set budgets.daily.soft_usd: $20.00") || !strings.Contains(out, "daily.soft_usd") {
		t.Fatalf("set: %d %s %s", code, out, errOut)
	}
	if _, errOut, code := runCLI(t, "", "budget", "set", "budgets.session.hard_tool_calls", "500"); code != 0 {
		t.Fatal(errOut)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"# my settings", "# keep me", "soft_usd: 20", "hard_tool_calls: 500"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config.yaml lacks %q:\n%s", want, b)
		}
	}
	g := config.NewLoader(dir, config.LoaderOptions{}).For("")
	if g.Budgets.Daily.CostUSD.Soft != 20 || g.Budgets.Session.ToolCalls.Hard != 500 || g.Mode != config.ModeGuard {
		t.Errorf("loaded %+v", g.Budgets)
	}

	for _, args := range [][]string{
		{"set", "weekly.soft_usd", "5"},           // unknown scope
		{"set", "daily.soft_usd", "0"},            // use clear
		{"set", "daily.soft_usd", "lots"},         // not a number
		{"set", "session.hard_tool_calls", "2.5"}, // whole numbers only
		{"set", "session.soft_tool_calls", "600"}, // above the hard limit
		{"clear", "daily.max_usd"},                // unknown key
		{"set", "daily.soft_usd"},                 // no value
	} {
		if _, _, code := runCLI(t, "", append([]string{"budget"}, args...)...); code == 0 {
			t.Errorf("budget %v accepted", args)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(b) {
		t.Errorf("a refused change edited the file:\n%s", after)
	}

	out, _, code = runCLI(t, "", "budget", "clear", "daily.soft_usd")
	if code != 0 || !strings.Contains(out, "Cleared budgets.daily.soft_usd") || strings.Contains(out, "daily.soft_usd   ") {
		t.Fatalf("clear: %s", out)
	}
	if g := config.NewLoader(dir, config.LoaderOptions{}).For(""); g.Budgets.Daily.CostUSD.On() || g.Budgets.Session.ToolCalls.Hard != 500 {
		t.Errorf("after clear %+v", g.Budgets)
	}
}
