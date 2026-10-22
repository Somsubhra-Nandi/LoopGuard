package dashboard

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.html")

// normalize removes what differs between machines: the temp directories
// and the path separator.
func (f *fixture) normalize(s string) string {
	s = strings.ReplaceAll(s, f.home, "<HOME>")
	s = strings.ReplaceAll(s, f.transcripts, "<TRANSCRIPTS>")
	return strings.ReplaceAll(s, `\`, "/")
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.html")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/dashboard -update)", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("%s differs; if intended, rerun with -update and review the diff.\ngot:\n%s", path, got)
	}
}

// TestPagesGolden renders every page of the fixture and compares it with
// its snapshot.
func TestPagesGolden(t *testing.T) {
	f := newFixture(t)
	c := f.login()
	tok := c.csrfToken()
	pages := []struct {
		name   string
		method string
		path   string
		form   url.Values
		want   []string // also checked without -update
	}{
		{"overview", "GET", "/ui/", nil, []string{"Last 30 days", "Loops caught", "inside loops", "<svg class=\"chart\"", "past the soft limit"}},
		{"sessions", "GET", "/ui/sessions", nil, []string{"shop", "api", "scratch", ">live<"}},
		{"sessions-by-cost", "GET", "/ui/sessions?sort=cost&from=&project=" + url.QueryEscape(repoAPI), nil, []string{"api"}},
		{"session", "GET", "/ui/sessions/" + sidShop, nil, []string{"class=\"loop\"", "in-loop", "Largest outputs", "real nudge", "Stop its turn"}},
		{"session-no-transcript", "GET", "/ui/sessions/" + sidAPI, nil, []string{"No transcript", "exact_repeat", "Resume repo"}},
		{"costs", "GET", "/ui/costs", nil, []string{"Top 4 by estimated cost", "read logs/big.log"}},
		{"settings", "GET", "/ui/settings", nil, []string{"api", "ntfy.sh/se••••", "daily.soft_usd", "Ids inferred"}},
		{"confirm-stop", "POST", "/ui/confirm", url.Values{"csrf": {tok}, "action": {actStop}, "target": {sidShop}, "back": {"/ui/sessions/" + sidShop}},
			[]string{"Stop session aaaa1111", "Cancel"}},
		{"not-found", "GET", "/ui/sessions/nope", nil, []string{"No such session"}},
	}
	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			r := c.do(p.method, p.path, p.form, nil)
			if r.Code != http.StatusOK && p.name != "not-found" {
				t.Fatalf("%s: %d\n%s", p.path, r.Code, r.Body)
			}
			body := f.normalize(r.Body.String())
			for _, w := range p.want {
				if !strings.Contains(body, w) && !strings.Contains(r.Body.String(), w) {
					t.Errorf("%s lacks %q", p.name, w)
				}
			}
			if strings.Contains(body, topic) || strings.Contains(body, "0123456789") || strings.Contains(body, "make the cart test pass") {
				t.Errorf("%s shows a secret, an output, or a prompt", p.name)
			}
			checkGolden(t, p.name, body)
		})
	}
}

// TestNumbersMatchReport: the overview's numbers are report's, from the
// same functions (the CLI test compares with `report --json` itself).
func TestNumbersMatchReport(t *testing.T) {
	f := newFixture(t)
	c := f.login()
	r := c.get("/ui/api/overview")
	var got overviewData
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, r.Body)
	}
	pricing, _ := cost.Status(f.home)
	want := report.BuildSummary(context.Background(), f.st, pricing, at, report.NudgeAt())
	gb, _ := json.Marshal(got.Summary.Periods)
	wb, _ := json.Marshal(want.Periods)
	if string(gb) != string(wb) {
		t.Errorf("periods differ:\n got %s\nwant %s", gb, wb)
	}
	// The fixture: 3 sessions in 30 days (not the doctor's), 2 in 7 days,
	// 1 today with its loop; an unpriced model.
	p := got.Summary.Periods
	if p[0].Sessions != 1 || p[1].Sessions != 2 || p[2].Sessions != 3 || p[0].Loops != 1 || p[2].UnpricedTokens == 0 {
		t.Errorf("periods %+v", p)
	}
	// The chart's days add up to the cost since its first day.
	var days float64
	for _, d := range got.Days {
		days += d.CostUSD
	}
	rows, _ := f.st.UsageRows(context.Background(), store.UsageFilter{Since: got.Days[0].Date})
	var direct float64
	for _, pr := range report.Price(rows, cost.DefaultPricing()) {
		if pr.Priced {
			direct += pr.USD
		}
	}
	if len(got.Days) != chartDays || !near(days, direct) || !near(days, p[2].CostUSD) {
		t.Errorf("chart %v, rows %v, 30d %v", days, direct, p[2].CostUSD)
	}
	// Every period's cost appears on the page as report prints it.
	html := c.get("/ui/").Body.String()
	for _, pr := range p {
		if !strings.Contains(html, cost.FormatUSD(pr.CostUSD)) || !strings.Contains(html, cost.FormatUSD(pr.LoopCostUSD)) {
			t.Errorf("overview lacks %s / %s", cost.FormatUSD(pr.CostUSD), cost.FormatUSD(pr.LoopCostUSD))
		}
	}

	// Sessions: the list's numbers, from report.ListSessions.
	var sl sessionsData
	if err := json.Unmarshal(c.get("/ui/api/sessions?from=").Body.Bytes(), &sl); err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, s := range sl.Rows {
		sum += s.CostUSD
		if s.SessionID == sidAPI && (len(s.Keys) != 2 || s.ToolCalls != 35) {
			t.Errorf("subagent not merged: %+v", s.SessionRow)
		}
	}
	if len(sl.Rows) != 3 || !near(sum, p[2].CostUSD) {
		t.Errorf("sessions %d, cost %v vs 30d %v", len(sl.Rows), sum, p[2].CostUSD)
	}

	// Session detail: the report is report.BuildSession's.
	var dd detailData
	if err := json.Unmarshal(c.get("/ui/api/sessions/"+sidShop).Body.Bytes(), &dd); err != nil {
		t.Fatal(err)
	}
	rep, err := report.BuildSession(context.Background(), f.st, f.transcripts, sidShop, cost.DefaultPricing())
	if err != nil {
		t.Fatal(err)
	}
	gj, _ := json.Marshal(dd.Report)
	wj, _ := json.Marshal(rep)
	if string(gj) != string(wj) || !dd.Report.Live || dd.Report.Totals.Loops != 1 {
		t.Errorf("session report differs:\n got %s\nwant %s", gj, wj)
	}
}

func near(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func TestActions(t *testing.T) {
	f := newFixture(t)
	c := f.login()
	tok := c.csrfToken()
	post := func(path string, v url.Values) (int, string) {
		v.Set("csrf", tok)
		r := c.do(http.MethodPost, path, v, nil)
		return r.Code, r.Body.String()
	}
	pauses := func() []store.Pause {
		ps, _ := f.st.Pauses(context.Background(), at)
		return ps
	}

	// The confirm step changes nothing.
	v := url.Values{"action": {actPauseSession}, "target": {sidShop}, "for": {"2h"}, "back": {"/ui/sessions/" + sidShop}}
	if code, body := post("/ui/confirm", v); code != 200 || !strings.Contains(body, "Pause it") || len(pauses()) != 1 {
		t.Fatalf("confirm: %d %d pauses\n%s", code, len(pauses()), body)
	}
	if code, body := post("/ui/action", v); code != 200 || !strings.Contains(body, "Paused session") || f.be.reloads != 1 {
		t.Fatalf("pause: %d %s", code, body)
	}
	if ps := pauses(); len(ps) != 2 || (ps[0].Until.Sub(at) != 2*time.Hour && ps[1].Until.Sub(at) != 2*time.Hour) {
		t.Errorf("pauses %+v", ps)
	}
	if _, body := post("/ui/action", url.Values{"action": {actResumeSession}, "target": {sidShop}}); !strings.Contains(body, "Resumed session") {
		t.Errorf("resume: %s", body)
	}
	if _, body := post("/ui/action", url.Values{"action": {actResumeRepo}, "target": {repoAPI}}); !strings.Contains(body, "Resumed repo") || len(pauses()) != 0 {
		t.Errorf("resume repo: %s", body)
	}
	if _, body := post("/ui/action", url.Values{"action": {actPauseRepo}, "target": {repoShop}, "for": {""}}); !strings.Contains(body, "until resumed") {
		t.Errorf("pause repo: %s", body)
	}
	if code, body := post("/ui/action", url.Values{"action": {actStop}, "target": {sidShop}}); code != 200 || len(f.be.stopped) != 1 {
		t.Errorf("stop: %d %s", code, body)
	}

	// Refused: unknown targets, an ended session's stop, odd lengths, and a
	// way out of the dashboard.
	for _, bad := range []url.Values{
		{"action": {actPauseSession}, "target": {"nope"}},
		{"action": {actPauseRepo}, "target": {"/etc"}},
		{"action": {actStop}, "target": {sidAPI}},
		{"action": {actPauseSession}, "target": {sidShop}, "for": {"9y"}},
		{"action": {"delete-everything"}, "target": {sidShop}},
		{"action": {actPauseSession}},
	} {
		if code, _ := post("/ui/action", bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, code)
		}
	}
	if _, body := post("/ui/confirm", url.Values{"action": {actPauseSession}, "target": {sidShop}, "back": {"//evil.example/x"}}); strings.Contains(body, "evil") {
		t.Error("back link left the dashboard")
	}
}

// TestTopCostsMatchOverview: Top costs counts the sessions the overview
// counts, over the same 7 days, so the two 7-day totals are equal; a
// transcript of a session LoopGuard doesn't know adds nothing.
func TestTopCostsMatchOverview(t *testing.T) {
	f := newFixture(t)
	// A session LoopGuard never saw, changed today: left out.
	dir := filepath.Join(f.transcripts, "C--elsewhere")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","timestamp":"` + at.Add(-time.Hour).UTC().Format(time.RFC3339) + `","message":{"id":"msg_x","model":"claude-opus-5-5","role":"assistant",` +
		`"content":[{"type":"text","text":"x"}],"usage":{"input_tokens":0,"output_tokens":5000000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ffff0000-0000-4000-8000-000000000009.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	c := f.login()
	var ov overviewData
	if err := json.Unmarshal(c.get("/ui/api/overview").Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	week := ov.Summary.Periods[1]
	est, err := f.d.topCostsCached()
	if err != nil {
		t.Fatal(err)
	}
	if week.Name != "7d" || week.CostUSD == 0 || est.CostUSD != week.CostUSD {
		t.Errorf("top costs total %v, overview 7 days %v", est.CostUSD, week.CostUSD)
	}
	if est.Streams != 1 {
		t.Errorf("top costs read %d streams, want 1 (the known session's)", est.Streams)
	}
	var shares float64
	for _, g := range est.Top {
		shares += g.EstShare
	}
	if shares > 1+1e-9 {
		t.Errorf("shares add up to %v", shares)
	}
	if html := c.get("/ui/costs").Body.String(); !strings.Contains(html, cost.FormatUSD(week.CostUSD)+" in all") {
		t.Errorf("costs page lacks the 7-day total %s", cost.FormatUSD(week.CostUSD))
	}
}
