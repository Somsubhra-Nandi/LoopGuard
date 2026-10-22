package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/dashboard"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// LoginLink is the POST /v1/dashboard/login reply: a one-time link that
// signs a browser in to the dashboard (`loopguard dashboard`).
type LoginLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (d *server) dashboardLogin(w http.ResponseWriter, _ *http.Request) {
	u, exp, err := d.dash.NewLoginLink()
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	d.log.Info("dashboard login link issued", "expires", exp)
	writeJSON(w, http.StatusOK, LoginLink{URL: u, ExpiresAt: exp})
}

// dashBackend is the daemon as the dashboard sees it.
type dashBackend struct{ d *server }

func (b dashBackend) Live() map[string]dashboard.Live {
	out := map[string]dashboard.Live{}
	for key, s := range b.d.mgr.Live() {
		out[key] = dashboard.Live{SessionID: s.SessionID, Mode: s.Mode, ModeWhy: s.ModeWhy, Ended: !s.EndedAt.IsZero()}
	}
	return out
}

func (b dashBackend) Level(sessionID, repo, cwd string) config.ModeInfo {
	m := b.d.mgr
	return m.sessionMode(m.cfg.For(repo), &event.Event{SessionID: sessionID, Cwd: cwd}, repo, time.Now())
}

func (b dashBackend) Config(repo string) config.Effective { return b.d.mgr.cfg.For(repo) }

func (b dashBackend) Pricing() (*cost.Pricing, cost.PricingStatus) { return cost.Status(b.d.home) }

func (b dashBackend) Usage() dashboard.Usage {
	ids := map[string]bool{}
	for _, s := range b.d.mgr.Live() {
		ids[s.SessionID] = true
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	r := b.d.mgr.book.reply(list, time.Now())
	u := dashboard.Usage{Today: viewOf(r.Today), Sessions: map[string]dashboard.UsageView{}}
	for id, v := range r.Sessions {
		u.Sessions[id] = viewOf(v)
	}
	return u
}

func viewOf(v UsageView) dashboard.UsageView {
	return dashboard.UsageView{Tokens: v.Tokens, CostUSD: v.CostUSD, UnpricedTokens: v.UnpricedTokens, ToolCalls: v.ToolCalls, WallMinutes: v.WallMinutes}
}

func (b dashBackend) Stop(sessionID string) dashboard.StopResult {
	code, r := b.d.requestStop(StopRequest{Session: sessionID})
	switch {
	case code != http.StatusOK:
		return dashboard.StopResult{Why: r.Error}
	case len(r.Stopped) == 0 && len(r.Refused) > 0:
		return dashboard.StopResult{Why: "it observes only (" + r.Refused[0].ModeWhy + "), and LoopGuard never touches the agent in observe"}
	case len(r.Stopped) == 0:
		return dashboard.StopResult{Why: "no active session " + sessionID}
	}
	return dashboard.StopResult{Stopped: true}
}

func (b dashBackend) ReloadPauses(ctx context.Context) error {
	return b.d.mgr.pauses.load(ctx, b.d.st, time.Now())
}

func (b dashBackend) Flush() {
	if b.d.w != nil {
		b.d.w.Flush()
	}
}
