package cost

import (
	"bytes"
	"encoding/json"
	"time"
)

// Usage is the token usage of one API message (LLD §8.1), counted the way
// the transcript reports it: Input excludes the cached prefix, CacheWrite
// is the prefix written to the prompt cache (CacheWrite1h of it to the
// one-hour cache, the rest to the five-minute one), CacheRead the prefix
// read from it. Output includes thinking.
type Usage struct {
	Input        int64 `json:"input_tokens"`
	Output       int64 `json:"output_tokens"`
	CacheWrite   int64 `json:"cache_write_tokens"`
	CacheRead    int64 `json:"cache_read_tokens"`
	CacheWrite1h int64 `json:"cache_write_1h_tokens,omitempty"`
}

// Total is every token the message processed: input, output, cache writes,
// and cache reads (the convention of Claude Code's own totals). Token
// budgets count this.
func (u Usage) Total() int64 { return u.Input + u.Output + u.CacheWrite + u.CacheRead }

// Prompt is the message's prompt length: input plus the cached prefix.
func (u Usage) Prompt() int64 { return u.Input + u.CacheWrite + u.CacheRead }

// Add returns the sum of u and v.
func (u Usage) Add(v Usage) Usage {
	return Usage{u.Input + v.Input, u.Output + v.Output, u.CacheWrite + v.CacheWrite, u.CacheRead + v.CacheRead,
		u.CacheWrite1h + v.CacheWrite1h}
}

// Sub returns u minus v (a delta between two readings of one message).
func (u Usage) Sub(v Usage) Usage {
	return Usage{u.Input - v.Input, u.Output - v.Output, u.CacheWrite - v.CacheWrite, u.CacheRead - v.CacheRead,
		u.CacheWrite1h - v.CacheWrite1h}
}

// Max returns the larger of each counter.
func (u Usage) Max(v Usage) Usage {
	return Usage{max(u.Input, v.Input), max(u.Output, v.Output), max(u.CacheWrite, v.CacheWrite),
		max(u.CacheRead, v.CacheRead), max(u.CacheWrite1h, v.CacheWrite1h)}
}

// IsZero reports whether every counter is 0.
func (u Usage) IsZero() bool { return u == Usage{} }

// Message is one API message's usage, as read from a transcript line.
type Message struct {
	ID    string
	Model string
	TS    time.Time
	Usage Usage
	// Speed ("fast" for fast mode) and Geo ("us" for US-only inference)
	// change the price (pricing.yaml multipliers).
	Speed, Geo string
}

// usageLine is the part of an assistant transcript line cost accounting
// reads.
type usageLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			Breakdown     *struct {
				OneHour int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
			Speed string `json:"speed"`
			Geo   string `json:"inference_geo"`
		} `json:"usage"`
	} `json:"message"`
}

// assistantMark is a cheap filter: lines without it are never assistant
// messages, so most lines (tool results, attachments) skip the decode.
var assistantMark = []byte(`"assistant"`)

// ParseLine reads the usage from one transcript line. It reports false for
// lines that aren't assistant messages with an id and usage. Unknown fields
// are ignored, so newer clients' lines still parse.
func ParseLine(b []byte) (Message, bool) {
	if !bytes.Contains(b, assistantMark) {
		return Message{}, false
	}
	var l usageLine
	if json.Unmarshal(b, &l) != nil || l.Type != "assistant" || l.Message == nil || l.Message.ID == "" || l.Message.Usage == nil {
		return Message{}, false
	}
	u := l.Message.Usage
	m := Message{
		ID:    l.Message.ID,
		Model: l.Message.Model,
		Usage: Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheCreation, CacheRead: u.CacheRead},
		Speed: u.Speed,
		Geo:   u.Geo,
	}
	if u.Breakdown != nil {
		m.Usage.CacheWrite1h = min(u.Breakdown.OneHour, u.CacheCreation)
	}
	if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		m.TS = t
	}
	return m, true
}

// SyntheticModel is the model of messages the client writes itself (an
// error shown as a reply); they carry no real usage.
const SyntheticModel = "<synthetic>"

// Ledger deduplicates messages by id: a message spread over several lines
// repeats its usage on each, and the counters can grow between lines, so
// the ledger keeps the largest value of each counter (LLD §8.1).
type Ledger struct {
	byID  map[string]int
	msgs  []Message
	total Usage
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger { return &Ledger{byID: map[string]int{}} }

// Add records a reading of a message and returns the message as it now
// stands (maxima so far) and how much the totals grew. A repeated line
// adds nothing.
func (l *Ledger) Add(m Message) (Message, Usage) {
	i, ok := l.byID[m.ID]
	if !ok {
		l.byID[m.ID] = len(l.msgs)
		l.msgs = append(l.msgs, m)
		l.total = l.total.Add(m.Usage)
		return m, m.Usage
	}
	cur := &l.msgs[i]
	next := cur.Usage.Max(m.Usage)
	delta := next.Sub(cur.Usage)
	cur.Usage = next
	if cur.Model == "" || cur.Model == SyntheticModel {
		cur.Model = m.Model
	}
	if cur.TS.IsZero() {
		cur.TS = m.TS
	}
	if m.Speed != "" {
		cur.Speed = m.Speed
	}
	if m.Geo != "" {
		cur.Geo = m.Geo
	}
	l.total = l.total.Add(delta)
	return *cur, delta
}

// Index returns the position of a message id in arrival order (-1 if
// unknown).
func (l *Ledger) Index(id string) int {
	if i, ok := l.byID[id]; ok {
		return i
	}
	return -1
}

// Messages returns the messages in the order they first appeared.
func (l *Ledger) Messages() []Message { return l.msgs }

// Len is the number of distinct messages.
func (l *Ledger) Len() int { return len(l.msgs) }

// Total is the deduplicated usage of all messages.
func (l *Ledger) Total() Usage { return l.total }
