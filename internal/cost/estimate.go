package cost

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"
	"unicode/utf8"
)

// CharsPerToken is the estimate of a tool result's size in tokens when no
// exact count is known: about 4 characters per token.
const CharsPerToken = 4

// Scanner reads one transcript stream's raw lines and accounts for its
// usage: messages deduplicated by id, per turn, and per tool call an
// estimate of what the call cost (Estimates). Feed it every line in order
// with Line; call MarkTurn when a human prompt starts a turn, and Label
// to name a tool call (its family or tool and target) and give its number.
type Scanner struct {
	ledger   *Ledger
	turnOf   []int // per message (ledger index): the turn it started in
	segOf    []int // per message: the compaction segment it started in
	turn     int
	seg      int
	uses     map[string]*toolUse
	order    []*toolUse
	perMsg   map[int]int // tool uses per message index
	lastMsg  int         // ledger index of the latest message (-1: none)
	compacts int
}

type toolUse struct {
	id      string
	name    string
	msg     int // issuing message (ledger index)
	result  bool
	chars   int64
	isError bool
	after   int // messages seen when the result arrived: later calls have index >= after
	seg     int
	group   string
	call    int
	turn    int
}

// NewScanner returns an empty scanner.
func NewScanner() *Scanner {
	return &Scanner{ledger: NewLedger(), uses: map[string]*toolUse{}, perMsg: map[int]int{}, lastMsg: -1}
}

// scanLine is the part of a line the scanner needs besides usage.
type scanLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message *struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type scanBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Text      string          `json:"text"`
	// Chars is the original result's length, in anonymized exports (whose
	// content is a hash token).
	Chars *int64 `json:"chars"`
}

var compactMark = []byte(`compact_boundary`)

// Line accounts for one raw transcript line. Lines it doesn't understand
// are ignored.
func (s *Scanner) Line(b []byte) {
	if m, ok := ParseLine(b); ok {
		s.message(m, b)
		return
	}
	switch {
	case bytes.Contains(b, []byte(`"tool_result"`)):
		s.results(b)
	case bytes.Contains(b, compactMark):
		var l scanLine
		if json.Unmarshal(b, &l) == nil && l.Type == "system" && l.Subtype == "compact_boundary" {
			s.seg++
			s.compacts++
		}
	}
}

func (s *Scanner) message(m Message, b []byte) {
	before := s.ledger.Len()
	s.ledger.Add(m)
	idx := s.ledger.Index(m.ID)
	if s.ledger.Len() > before {
		s.turnOf = append(s.turnOf, s.turn)
		s.segOf = append(s.segOf, s.seg)
	}
	s.lastMsg = idx
	if !bytes.Contains(b, []byte(`"tool_use"`)) {
		return
	}
	var l scanLine
	if json.Unmarshal(b, &l) != nil || l.Message == nil {
		return
	}
	var blocks []scanBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return
	}
	for _, bl := range blocks {
		if bl.Type != "tool_use" || bl.ID == "" || s.uses[bl.ID] != nil {
			continue
		}
		u := &toolUse{id: bl.ID, name: bl.Name, msg: idx, turn: s.turn}
		s.uses[bl.ID] = u
		s.order = append(s.order, u)
		s.perMsg[idx]++
	}
}

func (s *Scanner) results(b []byte) {
	var l scanLine
	if json.Unmarshal(b, &l) != nil || l.Type != "user" || l.Message == nil || len(l.Message.Content) == 0 || l.Message.Content[0] != '[' {
		return
	}
	var blocks []scanBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return
	}
	for _, bl := range blocks {
		if bl.Type != "tool_result" {
			continue
		}
		u := s.uses[bl.ToolUseID]
		if u == nil || u.result {
			continue
		}
		u.result, u.chars, u.isError = true, ResultChars(bl.Content), bl.IsError
		if bl.Chars != nil {
			u.chars = *bl.Chars
		}
		u.after, u.seg = s.ledger.Len(), s.seg
	}
}

// ResultChars is the length in characters of what the model saw of a
// tool result: a string, or the text blocks of an array (images and other
// blocks aren't counted).
func ResultChars(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	if raw[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return int64(utf8.RuneCountInString(str))
		}
		return 0
	}
	var bs []scanBlock
	if json.Unmarshal(raw, &bs) != nil {
		return 0
	}
	var n int64
	for _, b := range bs {
		if b.Type == "text" {
			n += int64(utf8.RuneCountInString(b.Text))
		}
	}
	return n
}

// MarkTurn starts a new turn: messages from now on belong to it. Turn 0 is
// what came before the first human prompt.
func (s *Scanner) MarkTurn() { s.turn++ }

// Turn is the current turn number.
func (s *Scanner) Turn() int { return s.turn }

// Label names a tool call for the estimates: its group (the command
// family, or the tool and its target) and its number in the stream.
func (s *Scanner) Label(toolUseID, group string, call int) {
	if u := s.uses[toolUseID]; u != nil {
		u.group, u.call = group, call
	}
}

// IssuingMessage returns the index of the message that issued a tool call
// (-1 if unknown), for CostBetween.
func (s *Scanner) IssuingMessage(toolUseID string) int {
	if u := s.uses[toolUseID]; u != nil {
		return u.msg
	}
	return -1
}

// LastMessage is the index of the latest message (-1 before any).
func (s *Scanner) LastMessage() int { return s.lastMsg }

// Ledger returns the deduplicated messages.
func (s *Scanner) Ledger() *Ledger { return s.ledger }

// ModelUsage is one model's share of a stream.
type ModelUsage struct {
	Model    string  `json:"model"`
	Messages int     `json:"messages"`
	Usage    Usage   `json:"usage"`
	CostUSD  float64 `json:"cost_usd"`
	Priced   bool    `json:"priced"`
}

// TurnCost is one turn's usage.
type TurnCost struct {
	Turn     int     `json:"turn"`
	Messages int     `json:"messages"`
	Usage    Usage   `json:"usage"`
	CostUSD  float64 `json:"cost_usd"`
	Unpriced int64   `json:"unpriced_tokens,omitempty"`
}

// CallEstimate is the estimated cost of one tool call (an estimate, see
// Scanner.Summary): its share of the output tokens of the message that
// issued it, plus its result carried as input by every later model call
// until the next compaction.
type CallEstimate struct {
	ToolUseID    string  `json:"-"`
	Group        string  `json:"group"`
	Call         int     `json:"call,omitempty"`
	Turn         int     `json:"turn"`
	OutputTokens float64 `json:"output_tokens"` // share of the issuing message's output
	ResultTokens int64   `json:"result_tokens"` // estimated from characters
	LaterCalls   int     `json:"later_calls"`   // model calls that carried the result
	EstUSD       float64 `json:"est_usd"`
	Priced       bool    `json:"priced"`
}

// Summary is a stream's usage and cost.
type Summary struct {
	Messages int          `json:"messages"`
	Usage    Usage        `json:"usage"`
	CostUSD  float64      `json:"cost_usd"` // priced messages only
	Unpriced Usage        `json:"unpriced_usage"`
	Models   []ModelUsage `json:"models"`
	Turns    []TurnCost   `json:"turns"`
	// Compactions counts compact_boundary lines.
	Compactions int `json:"compactions"`
	// Calls holds an estimate per tool call, in issue order. The estimates
	// are scaled down if needed so that they never add up to more than
	// CostUSD.
	Calls []CallEstimate `json:"calls"`
	// MessageCost is each message's cost (ledger order; 0 if unpriced).
	MessageCost []float64 `json:"-"`
	// MessageTimes are the times of the messages counted.
	MessageTimes []time.Time `json:"-"`
}

// Summarize prices the stream and estimates each tool call's cost.
func (s *Scanner) Summarize(p *Pricing) Summary {
	return s.SummarizeWindow(p, time.Time{}, time.Time{})
}

// SummarizeWindow is Summarize counting only the model calls made in
// [from, to) (a zero bound is open; a message without a time counts): the
// usage and cost, and per tool call what the calls in the window spent on
// it. Calls issued before the window whose results were carried in it are
// kept; others with nothing in it are left out.
func (s *Scanner) SummarizeWindow(p *Pricing, from, to time.Time) Summary {
	msgs := s.ledger.Messages()
	in := func(m Message) bool {
		return m.TS.IsZero() || ((from.IsZero() || !m.TS.Before(from)) && (to.IsZero() || m.TS.Before(to)))
	}
	sum := Summary{Compactions: s.compacts, MessageCost: make([]float64, len(msgs))}
	byModel := map[string]*ModelUsage{}
	turns := map[int]*TurnCost{}
	// carry[i] is what carrying one token of a result costs at message i:
	// the cache-read price when that call read from the cache, else the
	// input price.
	carry := make([]float64, len(msgs))
	for i, m := range msgs {
		if !in(m) {
			continue
		}
		sum.Messages++
		sum.Usage = sum.Usage.Add(m.Usage)
		sum.MessageTimes = append(sum.MessageTimes, m.TS)
		usd, ok := p.Cost(m)
		mu := byModel[m.Model]
		if mu == nil {
			mu = &ModelUsage{Model: m.Model, Priced: ok}
			byModel[m.Model] = mu
		}
		mu.Messages++
		mu.Usage = mu.Usage.Add(m.Usage)
		t := turns[s.turnOf[i]]
		if t == nil {
			t = &TurnCost{Turn: s.turnOf[i]}
			turns[s.turnOf[i]] = t
		}
		t.Messages++
		t.Usage = t.Usage.Add(m.Usage)
		if ok {
			sum.CostUSD += usd
			sum.MessageCost[i] = usd
			mu.CostUSD += usd
			t.CostUSD += usd
		} else {
			sum.Unpriced = sum.Unpriced.Add(m.Usage)
			t.Unpriced += m.Usage.Total()
		}
		if in, cr, ok := p.InputRate(m.Model); ok && m.Model != SyntheticModel {
			carry[i] = in
			if m.Usage.CacheRead > 0 {
				carry[i] = cr
			}
		}
	}
	for _, mu := range byModel {
		sum.Models = append(sum.Models, *mu)
	}
	sort.Slice(sum.Models, func(i, j int) bool { return sum.Models[i].Model < sum.Models[j].Model })
	for _, t := range turns {
		sum.Turns = append(sum.Turns, *t)
	}
	sort.Slice(sum.Turns, func(i, j int) bool { return sum.Turns[i].Turn < sum.Turns[j].Turn })

	// Suffix sums of carry, each ending at its compaction segment's end:
	// suffix[i] is what one token costs carried from message i to the next
	// compaction, over count[i] calls.
	suffix := make([]float64, len(msgs)+1)
	count := make([]int, len(msgs)+1)
	for i := len(msgs) - 1; i >= 0; i-- {
		if i+1 < len(msgs) && s.segOf[i+1] != s.segOf[i] {
			suffix[i], count[i] = carry[i], 1
		} else {
			suffix[i], count[i] = carry[i]+suffix[i+1], 1+count[i+1]
		}
	}

	var total float64
	for _, u := range s.order {
		e := CallEstimate{ToolUseID: u.id, Group: u.group, Call: u.call, Turn: u.turn}
		if e.Group == "" {
			e.Group = u.name
		}
		m := msgs[u.msg]
		issued := in(m)
		if n := s.perMsg[u.msg]; n > 0 && issued {
			e.OutputTokens = float64(m.Usage.Output) / float64(n)
		}
		outRate, okOut := p.OutputRate(m.Model)
		e.Priced = okOut
		// float64() keeps these products from fusing into FMAs; see Rates.cost.
		e.EstUSD = float64(e.OutputTokens * outRate)
		if u.result {
			e.ResultTokens = (u.chars + CharsPerToken - 1) / CharsPerToken
			// No later call in the result's segment: a compaction came first.
			if u.after < len(msgs) && s.segOf[u.after] == u.seg {
				e.LaterCalls = count[u.after]
				e.EstUSD += float64(float64(e.ResultTokens) * suffix[u.after])
			}
		}
		if !issued && e.EstUSD == 0 {
			continue
		}
		total += e.EstUSD
		sum.Calls = append(sum.Calls, e)
	}
	// Estimates can't add up to more than what the stream really cost.
	if total > sum.CostUSD && total > 0 {
		f := sum.CostUSD / total
		for i := range sum.Calls {
			sum.Calls[i].EstUSD *= f
		}
	}
	return sum
}

// CostBetween is the cost of messages from index from to index to,
// inclusive (the messages that issued a loop's first and last calls, and
// everything in between).
func (sum Summary) CostBetween(from, to int) float64 {
	if from < 0 || to < from {
		return 0
	}
	to = min(to, len(sum.MessageCost)-1)
	var c float64
	for i := from; i <= to; i++ {
		c += sum.MessageCost[i]
	}
	return c
}
