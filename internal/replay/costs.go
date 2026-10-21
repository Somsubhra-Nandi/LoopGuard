package replay

import (
	"sort"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
)

// CostBasis labels every cost LoopGuard shows: token usage priced at API
// rates. Subscription plans don't pay per token.
const CostBasis = "API-equivalent USD"

// SessionCost is one session's usage (main stream and subagents), and the
// part inside loops.
type SessionCost struct {
	SessionID      string     `json:"session_id"`
	Project        string     `json:"project"`
	Streams        int        `json:"streams"`
	Tokens         int64      `json:"tokens"`
	Usage          cost.Usage `json:"usage"`
	CostUSD        float64    `json:"cost_usd"`
	UnpricedTokens int64      `json:"unpriced_tokens"`
	WastedCostUSD  float64    `json:"wasted_cost_usd"`
}

// SessionCosts returns the usage per session, most expensive first.
func (c *Costs) SessionCosts() []SessionCost {
	by := map[string]*SessionCost{}
	for key, sc := range c.Streams {
		id, _, _ := strings.Cut(key, "/")
		s := by[id]
		if s == nil {
			s = &SessionCost{SessionID: id, Project: sc.Project}
			by[id] = s
		}
		s.Streams++
		s.Usage = s.Usage.Add(sc.Summary.Usage)
		s.Tokens = s.Usage.Total()
		s.CostUSD += sc.Summary.CostUSD
		s.UnpricedTokens += sc.Summary.Unpriced.Total()
		s.WastedCostUSD += sc.WastedUSD
	}
	out := make([]SessionCost, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// GroupEstimate is the estimated cost of one group of tool calls (a
// command family, or a tool and its target) across the streams: an
// estimate ("est."), see cost.CallEstimate.
type GroupEstimate struct {
	Group           string  `json:"group"`
	Runs            int     `json:"runs"`
	AvgResultTokens float64 `json:"avg_result_tokens"` // estimated, ~4 characters per token
	EstUSD          float64 `json:"est_usd"`
	// EstShare is EstUSD as a share of the real cost of the streams
	// counted. Estimates are capped per stream at the stream's real cost,
	// so the shares never add up to more than 1.
	EstShare float64 `json:"est_share"`
}

// OutputEstimate is one tool result's estimated cost.
type OutputEstimate struct {
	SessionID    string  `json:"session_id"`
	AgentID      string  `json:"agent_id,omitempty"`
	Group        string  `json:"group"`
	Call         int     `json:"call"`
	ResultTokens int64   `json:"result_tokens"`
	LaterCalls   int     `json:"later_calls"`
	EstUSD       float64 `json:"est_usd"`
	EstShare     float64 `json:"est_share"`
}

// Estimates ranks the tool calls of the streams for which keep is true
// (nil: all) by estimated cost: the top n groups, and the n single results
// that cost the most.
func (c *Costs) Estimates(n int, keep func(streamKey string) bool) (groups []GroupEstimate, largest []OutputEstimate) {
	total := 0.0
	by := map[string]*GroupEstimate{}
	resultTokens := map[string]int64{}
	keys := make([]string, 0, len(c.Streams))
	for k := range c.Streams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if keep != nil && !keep(key) {
			continue
		}
		sc := c.Streams[key]
		total += sc.Summary.CostUSD
		sess, agent, _ := strings.Cut(key, "/")
		for _, e := range sc.Summary.Calls {
			g := by[e.Group]
			if g == nil {
				g = &GroupEstimate{Group: e.Group}
				by[e.Group] = g
			}
			g.Runs++
			g.EstUSD += e.EstUSD
			resultTokens[e.Group] += e.ResultTokens
			largest = append(largest, OutputEstimate{SessionID: sess, AgentID: agent, Group: e.Group, Call: e.Call,
				ResultTokens: e.ResultTokens, LaterCalls: e.LaterCalls, EstUSD: e.EstUSD})
		}
	}
	for name, g := range by {
		g.AvgResultTokens = float64(resultTokens[name]) / float64(g.Runs)
		if total > 0 {
			g.EstShare = g.EstUSD / total
		}
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].EstUSD != groups[j].EstUSD {
			return groups[i].EstUSD > groups[j].EstUSD
		}
		return groups[i].Group < groups[j].Group
	})
	sort.SliceStable(largest, func(i, j int) bool { return largest[i].EstUSD > largest[j].EstUSD })
	if len(groups) > n {
		groups = groups[:n]
	}
	if len(largest) > 5 {
		largest = largest[:5]
	}
	for i := range largest {
		if total > 0 {
			largest[i].EstShare = largest[i].EstUSD / total
		}
	}
	if groups == nil {
		groups = []GroupEstimate{}
	}
	if largest == nil {
		largest = []OutputEstimate{}
	}
	return groups, largest
}
