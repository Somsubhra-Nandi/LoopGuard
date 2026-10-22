package report

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Session is `loopguard report <session>`.
type Session struct {
	Basis     string         `json:"basis"`
	SessionID string         `json:"session_id"`
	Project   string         `json:"project"`
	Live      bool           `json:"live_data"` // LoopGuard's database has the session (real interventions)
	Totals    SessionTotals  `json:"totals"`
	Streams   []Stream       `json:"streams"`
	Estimates EstimateReport `json:"estimates"`
}

// SessionTotals adds up a session's streams.
type SessionTotals struct {
	Calls          int        `json:"tool_calls"`
	Failures       int        `json:"failures"`
	Turns          int        `json:"turns"`
	Tokens         int64      `json:"tokens"`
	Usage          cost.Usage `json:"usage"`
	CostUSD        float64    `json:"cost_usd"`
	UnpricedTokens int64      `json:"unpriced_tokens"`
	UnknownModels  []string   `json:"unknown_models"`
	Loops          int        `json:"loops"`
	LoopSeconds    float64    `json:"loop_seconds"`
	LoopCostUSD    float64    `json:"loop_cost_usd"`
}

// Stream is the main session or one subagent.
type Stream struct {
	Stream   string  `json:"stream"` // "main" or the subagent id
	Calls    int     `json:"tool_calls"`
	Failures int     `json:"failures"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Turns    []Turn  `json:"turns"`
	// Other are LoopGuard's actions not tied to a loop found here (budget
	// limits, the user's stop, loops under other thresholds).
	Other []Action `json:"other_actions,omitempty"`
}

// Turn is the calls between two human prompts.
type Turn struct {
	Turn           int         `json:"turn"` // 0: before the first prompt
	StartSeconds   float64     `json:"start_seconds"`
	Calls          int         `json:"tool_calls"`
	Failures       int         `json:"failures"`
	Tokens         int64       `json:"tokens"`
	CostUSD        float64     `json:"cost_usd"`
	UnpricedTokens int64       `json:"unpriced_tokens,omitempty"`
	Groups         []CallGroup `json:"calls"`
	Loops          []Loop      `json:"loops"`
}

// CallGroup is a run of consecutive calls of one family or tool + target.
type CallGroup struct {
	From   int    `json:"from_call"`
	To     int    `json:"to_call"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
	Failed int    `json:"failed"`
}

// Loop is a loop the replay found, with what LoopGuard did about it.
type Loop struct {
	Detector     string   `json:"detector"`
	LoopKey      string   `json:"loop_key"`
	Subject      string   `json:"subject"`
	FirstCall    int      `json:"first_call"`
	LastCall     int      `json:"last_call"`
	Count        int      `json:"count"`
	StartSeconds float64  `json:"start_seconds"`
	EndSeconds   float64  `json:"end_seconds"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
	Actions      []Action `json:"actions"`
	Outcome      string   `json:"outcome"`
}

// Action is one thing LoopGuard did (or would have done) about a loop:
// from the database when it watched the session live ("real" or "would"),
// else from the replay ("replay").
type Action struct {
	Action   string  `json:"action"`
	Kind     string  `json:"kind"` // real | would | replay
	Call     int     `json:"call,omitempty"`
	AtSecond float64 `json:"at_seconds,omitempty"`
	Detector string  `json:"detector,omitempty"`
	Outcome  string  `json:"outcome,omitempty"`
	Reason   string  `json:"reason,omitempty"`
}

// ErrNoTranscript means no transcript under the path matches the session.
var ErrNoTranscript = errors.New("no transcript")

// BuildSession reports one session (by id or its first characters) from
// its transcript under path, with what LoopGuard did live from st (nil:
// no database).
func BuildSession(ctx context.Context, st *store.Store, path, prefix string, pricing *cost.Pricing) (*Session, error) {
	streams, err := replay.Discover(path, replay.Filter{Session: prefix})
	if err != nil {
		return nil, fmt.Errorf("reading transcripts: %w", err)
	}
	ids := map[string]bool{}
	for _, s := range streams {
		ids[s.SessionID] = true
	}
	switch len(ids) {
	case 0:
		return nil, fmt.Errorf("%w for a session starting with %q under %s", ErrNoTranscript, prefix, path)
	case 1:
	default:
		return nil, fmt.Errorf("%q matches %d sessions (%s); give more of the id", prefix, len(ids), strings.Join(SortedKeys(ids), ", "))
	}
	res := replay.Run(streams, replay.Options{Thresholds: config.Default(), Timeline: true, Pricing: pricing})
	if len(res.Errors) > 0 {
		return nil, errors.New(res.Errors[0])
	}
	r := &Session{Basis: replay.CostBasis, SessionID: streams[0].SessionID, Project: streams[0].Project}

	// What LoopGuard did live, if it watched the session.
	live := map[string][]store.Intervention{} // by stream key ("" main, else the agent id)
	if st != nil {
		rows, _ := st.SessionStreams(ctx, r.SessionID)
		for _, s := range rows {
			r.Live = true
			_, agent, _ := strings.Cut(s.Key[strings.IndexByte(s.Key, ':')+1:], "/")
			ivs, _ := st.Interventions(ctx, s.Key, 10000)
			live[agent] = ivs
		}
	}

	var t0 time.Time
	for _, tl := range res.Timelines {
		if len(tl) > 0 && (t0.IsZero() || tl[0].At.Before(t0)) {
			t0 = tl[0].At
		}
	}
	since := func(t time.Time) float64 {
		if t.IsZero() || t0.IsZero() {
			return 0
		}
		return t.Sub(t0).Seconds()
	}
	unknown := map[string]bool{}
	sort.Slice(streams, func(i, j int) bool { return streams[i].AgentID < streams[j].AgentID }) // main first
	for _, st := range streams {
		key := st.Key()
		sc := res.Costs.Streams[key]
		if sc == nil {
			continue
		}
		sum := sc.Summary
		sr := Stream{Stream: "main", Tokens: sum.Usage.Total(), CostUSD: sum.CostUSD, Turns: []Turn{}}
		if st.AgentID != "" {
			sr.Stream = st.AgentID
		}
		for _, m := range sum.Models {
			if !m.Priced && m.Model != cost.SyntheticModel {
				unknown[m.Model] = true
			}
		}
		turns := map[int]*Turn{}
		turnOf := func(n int) *Turn {
			t := turns[n]
			if t == nil {
				t = &Turn{Turn: n, Groups: []CallGroup{}, Loops: []Loop{}}
				turns[n] = t
			}
			return t
		}
		for _, tc := range sum.Turns {
			t := turnOf(tc.Turn)
			t.Tokens, t.CostUSD, t.UnpricedTokens = tc.Usage.Total(), tc.CostUSD, tc.Unpriced
		}
		callTurn := map[int]int{}
		for _, c := range res.Timelines[key] {
			t := turnOf(c.Turn)
			callTurn[c.N] = c.Turn
			if t.Calls == 0 {
				t.StartSeconds = since(c.At)
			}
			t.Calls++
			sr.Calls++
			label := CallLabel(c)
			g := len(t.Groups) - 1
			if g < 0 || t.Groups[g].Label != label || t.Groups[g].To != c.N-1 {
				t.Groups = append(t.Groups, CallGroup{From: c.N, Label: label})
				g++
			}
			t.Groups[g].To = c.N
			t.Groups[g].Count++
			if c.Failed {
				t.Groups[g].Failed++
				t.Failures++
				sr.Failures++
			}
		}
		ivs := live[st.AgentID]
		used := map[int]bool{}
		for _, l := range res.Loops {
			if l.SessionID != st.SessionID || l.AgentID != st.AgentID {
				continue
			}
			lr := Loop{Detector: l.Detector, LoopKey: l.LoopKey, Subject: subject(l), FirstCall: l.FirstCall,
				LastCall: l.LastCall, Count: l.Count, StartSeconds: since(l.StartedAt), EndSeconds: since(l.EndedAt),
				CostUSD: l.CostUSD, Actions: []Action{}}
			if r.Live {
				for i, iv := range ivs {
					if iv.LoopKey == l.LoopKey && !used[i] {
						used[i] = true
						lr.Actions = append(lr.Actions, liveAction(iv, since))
					}
				}
			} else {
				for _, w := range l.WouldHave {
					lr.Actions = append(lr.Actions, Action{Action: w.Action, Kind: "replay", Call: w.Call})
				}
			}
			lr.Outcome = loopOutcome(lr, l)
			turnOf(callTurn[l.FirstCall]).Loops = append(turnOf(callTurn[l.FirstCall]).Loops, lr)
			r.Totals.Loops++
			r.Totals.LoopSeconds += l.DurationSeconds
		}
		r.Totals.LoopCostUSD += sc.WastedUSD
		for i, iv := range ivs {
			if !used[i] {
				sr.Other = append(sr.Other, liveAction(iv, since))
			}
		}
		for _, t := range turns {
			sr.Turns = append(sr.Turns, *t)
		}
		sort.Slice(sr.Turns, func(i, j int) bool { return sr.Turns[i].Turn < sr.Turns[j].Turn })
		r.Streams = append(r.Streams, sr)
		r.Totals.Calls += sr.Calls
		r.Totals.Failures += sr.Failures
		r.Totals.Usage = r.Totals.Usage.Add(sum.Usage)
		r.Totals.CostUSD += sum.CostUSD
		r.Totals.UnpricedTokens += sum.Unpriced.Total()
		if st.AgentID == "" {
			r.Totals.Turns = len(sr.Turns)
		}
	}
	r.Totals.Tokens = r.Totals.Usage.Total()
	r.Totals.UnknownModels = SortedKeys(unknown)
	top, largest := res.Costs.Estimates(TopEstimates, nil)
	r.Estimates = EstimateReport{Window: "this session", Streams: len(streams), CostUSD: res.Costs.CostUSD, Top: top, Largest: largest}
	return r, nil
}

// CallLabel names a call for the timeline: the command family, or the
// tool and its file (relative, redacted).
func CallLabel(c replay.Call) string {
	switch {
	case c.Family != "" && c.Tool == "bash":
		return "bash: " + c.Family
	case c.File != "":
		return c.Tool + " " + redact.String(c.File)
	}
	return c.Tool
}

func subject(l *replay.Loop) string {
	switch {
	case l.File != "":
		return redact.String(l.File)
	case l.Family != "":
		return "`" + l.Family + "`"
	}
	return l.LoopKey
}

func liveAction(iv store.Intervention, since func(time.Time) float64) Action {
	kind := "would"
	if iv.Mode == store.ModeEnforce {
		kind = "real"
	}
	outcome := iv.Outcome
	if outcome == "" {
		outcome = "pending"
	}
	return Action{Action: iv.Action, Kind: kind, AtSecond: since(iv.TS), Detector: iv.Detector, Outcome: outcome,
		Reason: Clip(FirstLine(redact.String(iv.Reason)), 120)}
}

// loopOutcome sums up how a loop ended: the outcome LoopGuard recorded
// for its last action, or, from the replay alone, "halted" / "went on".
func loopOutcome(lr Loop, l *replay.Loop) string {
	for i := len(lr.Actions) - 1; i >= 0; i-- {
		if o := lr.Actions[i].Outcome; o != "" && o != "pending" {
			return o
		}
	}
	for _, w := range l.WouldHave {
		if w.Action == "halt" {
			return "would have halted"
		}
	}
	switch {
	case len(lr.Actions) == 0:
		return "no action"
	case lr.Actions[0].Kind == "replay":
		return "not tracked (replay)"
	}
	return "pending"
}

// FirstLine is s's first non-blank line, trimmed.
func FirstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// Clip shortens s to n characters, ending in "…".
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
