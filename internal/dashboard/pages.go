package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Page limits.
const (
	chartDays   = 30
	topCacheTTL = 2 * time.Minute // the transcripts' 7-day estimates
	pageTimeout = 20 * time.Second
	noStoreNote = "The database is unavailable (the daemon runs memory-only), so there is no history to show."
)

func (d *Dashboard) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	if d.o.Backend != nil {
		d.o.Backend.Flush()
	}
	return context.WithTimeout(r.Context(), pageTimeout)
}

func (d *Dashboard) pricing() *cost.Pricing {
	if d.o.Backend == nil {
		return cost.DefaultPricing()
	}
	p, _ := d.o.Backend.Pricing()
	return p
}

// --- Overview ---

type overviewData struct {
	Summary    report.Summary `json:"summary"`
	Days       []report.Day   `json:"days"`
	Budgets    []budgetRow    `json:"budgets"`
	BudgetsSet bool           `json:"budgets_set"`
	Chart      chart          `json:"-"`
	Example    string         `json:"-"`
	AsOf       string         `json:"-"`
}

// budgetRow is one budget in effect against what it counts now.
type budgetRow struct {
	Scope  string  `json:"scope"` // "today", or a live session
	Key    string  `json:"key"`   // daily.soft_usd …
	Metric string  `json:"metric"`
	Used   float64 `json:"used"`
	Soft   float64 `json:"soft,omitempty"`
	Hard   float64 `json:"hard,omitempty"`
	State  string  `json:"state"` // ok | soft | hard
	// Formatted for the page.
	UsedText, SoftText, HardText string  `json:"-"`
	MeterMax                     float64 `json:"-"`
}

func (d *Dashboard) buildOverview(ctx context.Context) overviewData {
	s := d.snapshot(ctx)
	o := overviewData{Summary: s.summary, Days: s.days, Chart: newChart(s.days), Example: config.ExampleBudget, AsOf: d.asOf(s)}
	if d.o.Backend != nil {
		b := d.o.Backend.Config("").Budgets
		o.BudgetsSet = b.Any()
		o.Budgets = budgetRows(b, d.o.Backend.Usage(), d.o.Backend.Live())
	}
	if o.Budgets == nil {
		o.Budgets = []budgetRow{}
	}
	return o
}

func budgetRows(b config.Budgets, u Usage, live map[string]Live) []budgetRow {
	var out []budgetRow
	add := func(scopeName, scope string, view UsageView) {
		for _, m := range config.Metrics {
			l := b.Scope(scope).Get(m)
			if !l.On() {
				continue
			}
			r := budgetRow{Scope: scopeName, Metric: m, Used: view.Value(m), Soft: l.Soft, Hard: l.Hard, State: "ok"}
			key := config.BudgetKey(scope, m, "hard")
			if l.Soft > 0 {
				key = config.BudgetKey(scope, m, "soft")
			}
			r.Key = strings.TrimPrefix(key, "budgets.")
			switch {
			case l.Hard > 0 && r.Used > l.Hard:
				r.State = "hard"
			case l.Soft > 0 && r.Used > l.Soft:
				r.State = "soft"
			}
			r.UsedText, r.SoftText, r.HardText = budgetValue(m, r.Used), budgetValue(m, l.Soft), budgetValue(m, l.Hard)
			r.MeterMax = maxf(maxf(l.Hard, l.Soft), r.Used)
			out = append(out, r)
		}
	}
	add("today", config.ScopeDaily, u.Today)
	var ids []string
	for key, l := range live {
		if !strings.Contains(key, "/") && !l.Ended && l.SessionID != report.DoctorSession {
			ids = append(ids, l.SessionID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if v, ok := u.Sessions[id]; ok {
			add("session "+shortID(id), config.ScopeSession, v)
		}
	}
	return out
}

func budgetValue(metric string, v float64) string {
	switch {
	case v == 0:
		return "-"
	case metric == config.MetricCost:
		return cost.FormatUSD(v)
	case metric == config.MetricTokens:
		return report.CompactCount(int64(v))
	case metric == config.MetricWall:
		return report.ShortDuration(time.Duration(v * float64(time.Minute)))
	}
	return report.CompactCount(int64(v))
}

func (d *Dashboard) overview(w http.ResponseWriter, r *http.Request, s *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	o := d.buildOverview(ctx)
	d.render(w, http.StatusOK, "overview", view{Title: "Overview", Nav: "overview", CSRF: s.CSRF, Notices: d.notices(o.Summary.Errors), Data: o})
}

func (d *Dashboard) overviewJSON(w http.ResponseWriter, r *http.Request, _ *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	writeJSON(w, d.buildOverview(ctx))
}

func (d *Dashboard) notices(errs []string) []string {
	var n []string
	if d.o.Store == nil {
		n = append(n, noStoreNote)
	}
	return append(n, errs...)
}

// --- Sessions ---

type sessionsData struct {
	Rows      []sessionView `json:"sessions"`
	Projects  []string      `json:"-"`
	Project   string        `json:"-"`
	From, To  string        `json:"-"` // yyyy-mm-dd
	Sort      string        `json:"-"`
	Truncated bool          `json:"truncated"`
	AsOf      string        `json:"-"` // "": computed for this page
}

type sessionView struct {
	report.SessionRow
	Active   bool   `json:"active"`
	Level    string `json:"level"`
	LevelWhy string `json:"level_why"`
	LevelNow bool   `json:"level_is_current"` // not live: the level its next event would get
	Name     string `json:"-"`
	Started  string `json:"-"`
	Duration string `json:"-"`
}

const sessionLimit = 1000

func (d *Dashboard) buildSessions(ctx context.Context, q url.Values) (sessionsData, error) {
	at := d.now()
	sd := sessionsData{Project: q.Get("project"), Sort: q.Get("sort"), Rows: []sessionView{}, Projects: []string{}}
	// No from: the last 30 days (the snapshot's); an empty one: all.
	snap := d.snapshot(ctx)
	from := snap.from
	if q.Has("from") {
		from, _ = parseDay(q.Get("from"), at.Location())
	}
	to, _ := parseDay(q.Get("to"), at.Location())
	if !from.IsZero() {
		sd.From = from.Format("2006-01-02")
	}
	if !to.IsZero() {
		sd.To = to.Format("2006-01-02")
		to = to.AddDate(0, 0, 1) // through the end of that day
	}
	if d.o.Store == nil {
		return sd, nil
	}
	var rows []report.SessionRow
	var err error
	if from.Equal(snap.from) && to.IsZero() {
		rows, err, sd.AsOf = snap.sessions, snap.err, d.asOf(snap)
	} else {
		rows, err = report.ListSessions(ctx, d.o.Store, d.pricing(), report.SessionFilter{Since: from, From: from, Until: to, Limit: sessionLimit + 1},
			report.NudgeAt())
	}
	if err != nil {
		return sd, err
	}

	if len(rows) > sessionLimit {
		rows, sd.Truncated = rows[:sessionLimit], true
	}
	projects := map[string]bool{}
	live := d.live()
	for _, row := range rows {
		projects[row.Project] = true
		if sd.Project != "" && row.Project != sd.Project {
			continue
		}
		sd.Rows = append(sd.Rows, d.sessionView(row, live))
	}
	sd.Projects = report.SortedKeys(projects)
	switch sd.Sort {
	case "cost":
		sort.SliceStable(sd.Rows, func(i, j int) bool { return sd.Rows[i].CostUSD > sd.Rows[j].CostUSD })
	case "loops":
		sort.SliceStable(sd.Rows, func(i, j int) bool { return sd.Rows[i].Loops > sd.Rows[j].Loops })
	default:
		sd.Sort = "date"
		sort.SliceStable(sd.Rows, func(i, j int) bool { return sd.Rows[i].StartedAt.After(sd.Rows[j].StartedAt) })
	}
	return sd, nil
}

func (d *Dashboard) live() map[string]Live {
	if d.o.Backend == nil {
		return map[string]Live{}
	}
	return d.o.Backend.Live()
}

func (d *Dashboard) sessionView(row report.SessionRow, live map[string]Live) sessionView {
	v := sessionView{SessionRow: row, Name: baseName(row.Project), Started: row.StartedAt.In(d.o.Location).Format("2006-01-02 15:04"),
		Duration: report.ShortDuration(row.Duration())}
	if v.Name == "" {
		v.Name = shortID(row.SessionID)
	}
	if len(row.Keys) > 0 {
		if l, ok := live[row.Keys[0]]; ok && !l.Ended {
			v.Active, v.Level, v.LevelWhy = true, l.Mode, l.ModeWhy
		}
	}
	if !v.Active && d.o.Backend != nil {
		m := d.o.Backend.Level(row.SessionID, row.Repo, row.Cwd)
		v.Level, v.LevelWhy, v.LevelNow = m.Mode, m.Why, true
	}
	return v
}

func parseDay(s string, loc *time.Location) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty")
	}
	return time.ParseInLocation("2006-01-02", s, loc)
}

func (d *Dashboard) sessionList(w http.ResponseWriter, r *http.Request, s *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	sd, err := d.buildSessions(ctx, r.URL.Query())
	var errs []string
	if err != nil {
		errs = append(errs, err.Error())
	}
	d.render(w, http.StatusOK, "sessions", view{Title: "Sessions", Nav: "sessions", CSRF: s.CSRF, Notices: d.notices(errs), Data: sd})
}

func (d *Dashboard) sessionListJSON(w http.ResponseWriter, r *http.Request, _ *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	sd, err := d.buildSessions(ctx, r.URL.Query())
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, sd)
}

// --- Session detail ---

type detailData struct {
	ID          string               `json:"session_id"`
	Found       bool                 `json:"found"` // in LoopGuard's database
	Row         sessionView          `json:"session"`
	Report      *report.Session      `json:"report,omitempty"`
	ReportError string               `json:"report_error,omitempty"`
	Streams     []streamView         `json:"-"`
	Paused      string               `json:"paused,omitempty"` // the session's own pause, in words
	RepoPaused  string               `json:"repo_paused,omitempty"`
	CanStop     bool                 `json:"can_stop"`
	CSRF        string               `json:"-"`
	Back        string               `json:"-"`
	Buttons     buttons              `json:"-"`
	LastSeen    string               `json:"-"`
	Other       []store.Intervention `json:"-"` // without a transcript: what the database has
}

type streamView struct {
	Name string
	report.Stream
	Turns []turnView
}

type turnView struct {
	report.Turn
	Head  string
	Items []timelineItem
}

// timelineItem is a group of calls, or a loop span placed before its
// first call.
type timelineItem struct {
	Group  *report.CallGroup
	Loop   *report.Loop
	InLoop bool
}

func (d *Dashboard) buildDetail(ctx context.Context, id string) (detailData, error) {
	dd := detailData{ID: id, Back: "/ui/sessions/" + url.PathEscape(id)}
	p := d.pricing()
	if d.o.Store != nil {
		row, ok, err := report.SessionByID(ctx, d.o.Store, p, id, report.NudgeAt())
		if err != nil {
			return dd, err
		}
		if ok {
			dd.Found = true
			dd.Row = d.sessionView(row, d.live())
			dd.LastSeen = row.LastSeenAt.In(d.o.Location).Format("2006-01-02 15:04")
			dd.CanStop = dd.Row.Active && config.LevelRank(dd.Row.Level) > config.LevelRank(config.ModeObserve)
			ps, err := d.o.Store.Pauses(ctx, d.o.Now())
			if err == nil {
				for _, ps := range ps {
					switch {
					case ps.Scope == store.PauseSession && ps.Target == id:
						dd.Paused = pauseText(ps, d.o.Location)
					case ps.Scope == store.PauseRepo && row.Repo != "" && ps.Target == row.Repo:
						dd.RepoPaused = pauseText(ps, d.o.Location)
					}
				}
			}
		}
	}
	rep, err := report.BuildSession(ctx, d.o.Store, d.o.Transcripts, id, p)
	if err != nil {
		dd.ReportError = err.Error()
		if errors.Is(err, report.ErrNoTranscript) {
			dd.ReportError = "No transcript of this session under " + d.o.Transcripts + ", so there is no timeline."
		}
		if dd.Found {
			for _, k := range dd.Row.Keys {
				ivs, _ := d.o.Store.Interventions(ctx, k, 200)
				dd.Other = append(dd.Other, ivs...)
			}
		}
	} else {
		dd.Report = rep
		dd.Streams = timeline(rep)
	}
	if !dd.Found && dd.Report == nil {
		return dd, errNotFound
	}
	return dd, nil
}

var errNotFound = errors.New("no such session")

// timeline places each loop before the first call group it covers and
// marks the groups inside a loop.
func timeline(r *report.Session) []streamView {
	var out []streamView
	for _, s := range r.Streams {
		sv := streamView{Name: "Main session", Stream: s}
		if s.Stream != "main" {
			sv.Name = "Subagent " + s.Stream
		}
		for _, t := range s.Turns {
			tv := turnView{Turn: t, Head: "Turn " + itoa(t.Turn)}
			if t.Turn == 0 {
				tv.Head = "Before the first prompt"
			}
			placed := make([]bool, len(t.Loops))
			for gi := range t.Groups {
				g := &t.Groups[gi]
				in := false
				for li := range t.Loops {
					l := &t.Loops[li]
					if !placed[li] && l.FirstCall <= g.To {
						placed[li] = true
						tv.Items = append(tv.Items, timelineItem{Loop: l})
					}
					if g.From <= l.LastCall && g.To >= l.FirstCall {
						in = true
					}
				}
				tv.Items = append(tv.Items, timelineItem{Group: g, InLoop: in})
			}
			for li := range t.Loops {
				if !placed[li] {
					tv.Items = append(tv.Items, timelineItem{Loop: &t.Loops[li]})
				}
			}
			sv.Turns = append(sv.Turns, tv)
		}
		out = append(out, sv)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func (d *Dashboard) sessionDetail(w http.ResponseWriter, r *http.Request, s *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	dd, err := d.buildDetail(ctx, id)
	switch {
	case errors.Is(err, errNotFound):
		d.render(w, http.StatusNotFound, "message", view{Title: "No such session", Nav: "sessions", CSRF: s.CSRF,
			Data: message{Title: "No such session", Text: "LoopGuard has no session " + id + ", and there is no transcript of it.", Back: "/ui/sessions"}})
		return
	case err != nil:
		d.render(w, http.StatusInternalServerError, "message", view{Title: "Error", Nav: "sessions", CSRF: s.CSRF,
			Data: message{Title: "Error", Text: err.Error(), Back: "/ui/sessions"}})
		return
	}
	dd.CSRF = s.CSRF
	dd.Buttons = sessionButtons(s.CSRF, dd.Back, dd)
	title := "Session " + shortID(id)
	d.render(w, http.StatusOK, "session", view{Title: title, Nav: "sessions", CSRF: s.CSRF, Notices: d.notices(nil), Data: dd})
}

func (d *Dashboard) sessionDetailJSON(w http.ResponseWriter, r *http.Request, _ *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	dd, err := d.buildDetail(ctx, r.PathValue("id"))
	switch {
	case errors.Is(err, errNotFound):
		http.Error(w, "no such session", http.StatusNotFound)
	case err != nil:
		http.Error(w, "store error", http.StatusInternalServerError)
	default:
		writeJSON(w, dd)
	}
}

func pauseText(p store.Pause, loc *time.Location) string {
	if p.Until.IsZero() {
		return "paused until resumed"
	}
	return "paused until " + p.Until.In(loc).Format("2006-01-02 15:04")
}

// --- Top costs ---

type costsData struct {
	Estimates *report.EstimateReport
	Error     string
}

// topCostsCached is report.TopCosts, kept for topCacheTTL: it reads the
// transcripts of the last 7 days.
func (d *Dashboard) topCostsCached() (*report.EstimateReport, error) {
	d.topMu.Lock()
	defer d.topMu.Unlock()
	now := d.o.Now()
	if d.topCost == nil && d.topErr == nil || now.Sub(d.topAt) > topCacheTTL {
		d.topCost, d.topErr = report.TopCosts(d.o.Transcripts, d.pricing(), d.now())
		d.topAt = now
	}
	return d.topCost, d.topErr
}

func (d *Dashboard) topCosts(w http.ResponseWriter, r *http.Request, s *uiSession) {
	est, err := d.topCostsCached()
	cd := costsData{Estimates: est}
	if err != nil {
		cd.Error = "Reading the transcripts under " + d.o.Transcripts + ": " + err.Error()
	}
	d.render(w, http.StatusOK, "costs", view{Title: "Top costs", Nav: "costs", CSRF: s.CSRF, Data: cd})
}

// --- Settings ---

type settingsData struct {
	Global     levelRow
	Repos      []levelRow
	Detectors  []detectorRow
	Policy     []kv
	Budgets    []kv
	Example    string
	Notify     []channelRow
	NotifyNote string
	Pricing    cost.PricingStatus
	Config     config.Loaded
	ConfigErr  string
	Pauses     []string
	Version    string
	Home       string
	CSRF       string
}

type levelRow struct {
	Repo, Name, Mode, Why string
	File                  string
	Notes                 []string
	Paused                bool
	Buttons               buttons
}

type detectorRow struct {
	Name     string
	Enabled  bool
	Settings string
}

type kv struct{ Key, Value string }

type channelRow struct {
	Name    string
	Enabled bool
	Target  string
	Events  string
}

func (d *Dashboard) settings(w http.ResponseWriter, r *http.Request, s *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	sd := settingsData{Version: d.o.Version, Home: d.o.Home, CSRF: s.CSRF, Example: config.ExampleBudget}
	b := d.o.Backend
	if b == nil {
		d.errorPage(w, r, http.StatusServiceUnavailable, "Settings unavailable", "The daemon's configuration is not available.")
		return
	}
	g := b.Config("")
	sd.Config = g.Global
	if g.Global.Err != nil {
		sd.ConfigErr = g.Global.Err.Error()
	}
	gm := b.Level("", "", "")
	sd.Global = levelRow{Name: "everywhere else", Mode: gm.Mode, Why: gm.Why, File: g.Global.Path}

	paused := map[string]bool{}
	if d.o.Store != nil {
		if ps, err := d.o.Store.Pauses(ctx, d.o.Now()); err == nil {
			for _, p := range ps {
				desc := "everything"
				switch p.Scope {
				case store.PauseSession:
					desc = "session " + shortID(p.Target)
				case store.PauseRepo:
					desc = "repo " + p.Target
					paused[p.Target] = true
				}
				sd.Pauses = append(sd.Pauses, desc+": "+pauseText(p, d.o.Location))
			}
		}
		repos := map[string]bool{}
		if rows, err := d.o.Store.ListSessions(ctx, store.ListOptions{Since: d.o.Now().Add(-30 * 24 * time.Hour), Limit: 100000}); err == nil {
			for _, row := range rows {
				if row.Repo != "" && row.SessionID != report.DoctorSession {
					repos[row.Repo] = true
				}
			}
		}
		for _, repo := range report.SortedKeys(repos) {
			e := b.Config(repo)
			m := b.Level("", repo, repo)
			lr := levelRow{Repo: repo, Name: baseName(repo), Mode: m.Mode, Why: m.Why, Notes: e.Notes, Paused: paused[repo]}
			if e.Repo.Exists {
				lr.File = e.Repo.Path
			}
			bt := button{Action: actPauseRepo, Target: repo, Label: "Pause", Back: "/ui/settings", Pause: true}
			if lr.Paused {
				bt = button{Action: actResumeRepo, Target: repo, Label: "Resume", Back: "/ui/settings"}
			}
			lr.Buttons = buttons{CSRF: s.CSRF, List: []button{bt}}
			sd.Repos = append(sd.Repos, lr)
		}
	}

	th := g.Thresholds
	dt := th.Detectors
	sd.Detectors = []detectorRow{
		{"exact_repeat (D1)", dt.ExactRepeat.Enabled, "min_count " + itoa(dt.ExactRepeat.MinCount)},
		{"repeated_failure (D2)", dt.RepeatedFailure.Enabled, "min_count " + itoa(dt.RepeatedFailure.MinCount) + ", max_progress " + ftoa(dt.RepeatedFailure.MaxProgress)},
		{"oscillation (D3)", dt.Oscillation.Enabled, "min_revisits " + itoa(dt.Oscillation.MinRevisits) + ", revisit_window " + itoa(dt.Oscillation.RevisitWindow)},
		{"read_thrash (D4)", dt.ReadThrash.Enabled, "min_reads " + itoa(dt.ReadThrash.MinReads)},
		{"stall (D5)", dt.Stall.Enabled, "window " + itoa(dt.Stall.Window)},
		{"budget (D6)", g.Budgets.Any(), "see Budgets"},
	}
	pt := th.Policy
	sd.Policy = []kv{
		{"window", itoa(dt.Window)},
		{"simhash_max_distance", itoa(dt.SimHashMaxDistance)},
		{"nudge_threshold", ftoa(pt.NudgeThreshold)},
		{"deny_after_nudges_occurrences", itoa(pt.DenyAfterNudgeOccurrences)},
		{"deny_min_severity", ftoa(pt.DenyMinSeverity)},
		{"halt_after_deny_occurrences", itoa(pt.HaltAfterDenyOccurrences)},
		{"halt_after_minutes", ftoa(pt.HaltAfterMinutes)},
		{"max_nudges_per_turn", itoa(pt.MaxNudgesPerTurn)},
		{"nudge_cooldown_calls", itoa(pt.NudgeCooldownCalls)},
		{"reset_quiet_calls", itoa(pt.ResetQuietCalls)},
		{"reset_progress", ftoa(pt.ResetProgress)},
		{"manual_mode_extra_occurrences", itoa(pt.ManualModeExtraOccurrences)},
		{"polling_families", strings.Join(th.PollingFamilies, ", ")},
		{"slow_families", strings.Join(th.SlowFamilies, ", ")},
	}
	for _, scope := range config.Scopes {
		for _, m := range config.Metrics {
			l := g.Budgets.Scope(scope).Get(m)
			for _, x := range []struct {
				level string
				v     float64
			}{{"soft", l.Soft}, {"hard", l.Hard}} {
				if x.v > 0 {
					sd.Budgets = append(sd.Budgets, kv{strings.TrimPrefix(config.BudgetKey(scope, m, x.level), "budgets."), budgetValue(m, x.v)})
				}
			}
		}
	}

	n := g.Notify
	for _, name := range []string{config.ChannelDesktop, config.ChannelTerminal, config.ChannelNtfy, config.ChannelWebhook} {
		ch := n.Channels()[name]
		on := ch.On
		if on == nil {
			on = n.On
		}
		row := channelRow{Name: name, Enabled: ch.Enabled, Events: strings.Join(on, ", ")}
		switch name {
		case config.ChannelNtfy:
			if ch.Topic != "" {
				row.Target = strings.TrimSuffix(ch.Server, "/") + "/" + maskSecret(ch.Topic)
			}
		case config.ChannelWebhook:
			row.Target = maskURL(ch.URL)
		}
		sd.Notify = append(sd.Notify, row)
	}
	if !config.Notifies(gm.Mode) {
		sd.NotifyNote = "Nothing is sent at the " + gm.Mode + " level."
	}
	_, sd.Pricing = b.Pricing()
	d.render(w, http.StatusOK, "settings", view{Title: "Settings", Nav: "settings", CSRF: s.CSRF, Notices: d.notices(nil), Data: sd})
}

// maskSecret shows the first two characters of a secret (an ntfy topic
// is one: anyone who knows it can read the messages).
func maskSecret(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "••••"
	}
	return string(r[:2]) + "••••"
}

// maskURL keeps a webhook URL's scheme and host (its path is the secret).
func maskURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "••••"
	}
	return u.Scheme + "://" + u.Host + "/••••"
}
