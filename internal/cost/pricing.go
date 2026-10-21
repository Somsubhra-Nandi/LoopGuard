package cost

import (
	"bytes"
	_ "embed" // the default pricing.yaml
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// PricingFile is the price table's name in the LoopGuard home.
const PricingFile = "pricing.yaml"

// Unverified is the `checked` value of a price nobody confirmed: its cost
// is shown as unknown, and doctor warns.
const Unverified = "unverified"

//go:embed pricing.yaml
var defaultPricing []byte

// DefaultPricingYAML returns the price table shipped in the binary.
func DefaultPricingYAML() []byte { return bytes.Clone(defaultPricing) }

// Rates are prices in USD per million tokens.
type Rates struct {
	Input        float64 `yaml:"input_per_mtok" json:"input_per_mtok"`
	Output       float64 `yaml:"output_per_mtok" json:"output_per_mtok"`
	CacheWrite   float64 `yaml:"cache_write_per_mtok" json:"cache_write_per_mtok"`
	CacheWrite1h float64 `yaml:"cache_write_1h_per_mtok" json:"cache_write_1h_per_mtok,omitempty"`
	CacheRead    float64 `yaml:"cache_read_per_mtok" json:"cache_read_per_mtok"`
}

// LongPrompt holds the prices for messages whose prompt is over
// AboveTokens (Claude Haiku 5.5).
type LongPrompt struct {
	AboveTokens int64 `yaml:"above_tokens" json:"above_tokens"`
	Rates       `yaml:",inline"`
}

// ModelPrice is one entry of the price table.
type ModelPrice struct {
	Match          string `yaml:"match" json:"match"`
	Rates          `yaml:",inline"`
	FastMultiplier float64     `yaml:"fast_multiplier" json:"fast_multiplier,omitempty"`
	USMultiplier   float64     `yaml:"us_multiplier" json:"us_multiplier,omitempty"`
	LongPrompt     *LongPrompt `yaml:"long_prompt" json:"long_prompt,omitempty"`
	Source         string      `yaml:"source" json:"source,omitempty"`
	Checked        string      `yaml:"checked" json:"checked"`
}

// Verified reports whether someone checked the price against a source.
func (m ModelPrice) Verified() bool {
	c := strings.TrimSpace(strings.ToLower(m.Checked))
	return c != "" && c != Unverified
}

// Pricing is the price table (pricing.yaml).
type Pricing struct {
	Currency        string       `yaml:"currency" json:"currency"`
	CurrencyDisplay string       `yaml:"currency_display" json:"currency_display,omitempty"`
	FXRate          float64      `yaml:"fx_rate" json:"fx_rate,omitempty"`
	Models          []ModelPrice `yaml:"models" json:"models"`
}

// ParsePricing decodes and checks a price table. Unknown keys are errors
// here (a typo in a price key would silently price at 0).
func ParsePricing(b []byte) (*Pricing, error) {
	var p Pricing
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	var errs []error
	if p.Currency != "USD" {
		errs = append(errs, fmt.Errorf("currency: %q: prices must be in USD (set currency_display and fx_rate to show another currency)", p.Currency))
	}
	if p.FXRate < 0 {
		errs = append(errs, fmt.Errorf("fx_rate: %g is negative", p.FXRate))
	}
	for i, m := range p.Models {
		if strings.TrimSpace(m.Match) == "" {
			errs = append(errs, fmt.Errorf("models[%d]: no match", i))
		} else if _, err := path.Match(m.Match, ""); err != nil {
			errs = append(errs, fmt.Errorf("models[%d]: match %q: %v", i, m.Match, err))
		}
		for _, r := range []Rates{m.Rates, longRates(m)} {
			if r.Input < 0 || r.Output < 0 || r.CacheWrite < 0 || r.CacheWrite1h < 0 || r.CacheRead < 0 {
				errs = append(errs, fmt.Errorf("models[%d] (%s): a price is negative", i, m.Match))
				break
			}
		}
		if m.FastMultiplier < 0 || m.USMultiplier < 0 {
			errs = append(errs, fmt.Errorf("models[%d] (%s): a multiplier is negative", i, m.Match))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &p, nil
}

func longRates(m ModelPrice) Rates {
	if m.LongPrompt == nil {
		return Rates{}
	}
	return m.LongPrompt.Rates
}

// DefaultPricing returns the shipped price table.
func DefaultPricing() *Pricing {
	p, err := ParsePricing(defaultPricing)
	if err != nil {
		panic("cost: the shipped pricing.yaml is invalid: " + err.Error())
	}
	return p
}

// dateSuffix is a model id's release date ("-20251001"); suffix is a
// variant in brackets ("[1m]").
var (
	dateSuffix    = regexp.MustCompile(`-\d{8}$`)
	bracketSuffix = regexp.MustCompile(`\[[^\]]*\]$`)
)

// Lookup returns the entry for a model: the first whose match equals the
// model id, or the id without its date or bracketed suffix, or matches
// either as a glob.
func (p *Pricing) Lookup(model string) (ModelPrice, bool) {
	if p == nil || model == "" {
		return ModelPrice{}, false
	}
	ids := []string{strings.ToLower(model)}
	base := bracketSuffix.ReplaceAllString(ids[0], "")
	base = dateSuffix.ReplaceAllString(base, "")
	if base != ids[0] {
		ids = append(ids, base)
	}
	for _, m := range p.Models {
		pat := strings.ToLower(strings.TrimSpace(m.Match))
		for _, id := range ids {
			if ok, _ := path.Match(pat, id); ok {
				return m, true
			}
		}
	}
	return ModelPrice{}, false
}

// Priced reports whether a model has a verified price.
func (p *Pricing) Priced(model string) bool {
	m, ok := p.Lookup(model)
	return ok && m.Verified()
}

// Cost prices one message in USD. ok is false when the model has no
// verified price (its cost is unknown, not 0).
func (p *Pricing) Cost(m Message) (usd float64, ok bool) {
	if m.Model == SyntheticModel || m.Usage.IsZero() {
		return 0, true
	}
	mp, found := p.Lookup(m.Model)
	if !found || !mp.Verified() {
		return 0, false
	}
	r := mp.Rates
	if lp := mp.LongPrompt; lp != nil && lp.AboveTokens > 0 && m.Usage.Prompt() > lp.AboveTokens {
		r = lp.Rates
	}
	mult := 1.0
	if strings.EqualFold(m.Speed, "fast") && mp.FastMultiplier > 0 {
		mult *= mp.FastMultiplier
	}
	if strings.EqualFold(m.Geo, "us") && mp.USMultiplier > 0 {
		mult *= mp.USMultiplier
	}
	return float64(mult * r.cost(m.Usage)), true
}

// cost is u priced at r, without multipliers. Each product is wrapped in
// float64() so the compiler can't fuse it with the sum into an FMA (Go
// allows that on arm64, ppc64 and s390x), which would change the last bits
// and make results differ by platform.
func (r Rates) cost(u Usage) float64 {
	oneHour := r.CacheWrite1h
	if oneHour == 0 {
		oneHour = r.CacheWrite
	}
	fiveMin := u.CacheWrite - u.CacheWrite1h
	return (float64(float64(u.Input)*r.Input) + float64(float64(u.Output)*r.Output) + float64(float64(fiveMin)*r.CacheWrite) +
		float64(float64(u.CacheWrite1h)*oneHour) + float64(float64(u.CacheRead)*r.CacheRead)) / 1e6
}

// InputRate and CacheReadRate return a model's per-token price of plain
// input and of a cache read, for estimates (0, false if not priced).
func (p *Pricing) InputRate(model string) (input, cacheRead float64, ok bool) {
	mp, found := p.Lookup(model)
	if !found || !mp.Verified() {
		return 0, 0, false
	}
	return mp.Input / 1e6, mp.CacheRead / 1e6, true
}

// OutputRate returns a model's per-token output price.
func (p *Pricing) OutputRate(model string) (float64, bool) {
	mp, found := p.Lookup(model)
	if !found || !mp.Verified() {
		return 0, false
	}
	return mp.Output / 1e6, true
}

// Unverified returns the entries whose price nobody checked.
func (p *Pricing) Unverified() []ModelPrice {
	var out []ModelPrice
	for _, m := range p.Models {
		if !m.Verified() {
			out = append(out, m)
		}
	}
	return out
}

// Format shows a USD amount, with the display currency next to it when
// one is set ("$1.23 (INR 103.32)").
func (p *Pricing) Format(usd float64) string {
	s := FormatUSD(usd)
	if p != nil && p.CurrencyDisplay != "" && p.CurrencyDisplay != "USD" && p.FXRate > 0 {
		s += fmt.Sprintf(" (%s %.2f)", p.CurrencyDisplay, usd*p.FXRate)
	}
	return s
}

// FormatUSD shows a USD amount with cents, or more precision under a cent.
func FormatUSD(usd float64) string {
	switch {
	case usd == 0:
		return "$0.00"
	case usd < 0.01 && usd > -0.01:
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// LoadPricing reads pricing.yaml from the LoopGuard home. A missing file
// gives the shipped table (exists false); an invalid one gives the shipped
// table and the error, so a typo never stops cost accounting.
func LoadPricing(home string) (p *Pricing, exists bool, err error) {
	b, err := os.ReadFile(filepath.Join(home, PricingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultPricing(), false, nil
	}
	if err != nil {
		return DefaultPricing(), true, err
	}
	p, err = ParsePricing(b)
	if err != nil {
		return DefaultPricing(), true, fmt.Errorf("%s: %w", PricingFile, err)
	}
	return p, true, nil
}

// EnsurePricing writes the shipped pricing.yaml into the home if there is
// none (never over an existing file). It reports whether it wrote one.
func EnsurePricing(home string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(home, PricingFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(defaultPricing); err != nil {
		f.Close()
		return true, err
	}
	return true, f.Close()
}

// PricingLoader re-reads pricing.yaml when it changes (checked at most
// every two seconds). It is safe for concurrent use.
type PricingLoader struct {
	home    string
	OnError func(error) // called when a changed file is invalid

	mu      sync.Mutex
	checked time.Time
	mod     time.Time
	size    int64
	p       *Pricing
	loaded  bool
}

// NewPricingLoader returns a loader for the home's pricing.yaml.
func NewPricingLoader(home string) *PricingLoader { return &PricingLoader{home: home} }

// Get returns the current price table.
func (l *PricingLoader) Get() *Pricing {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.loaded && now.Sub(l.checked) < 2*time.Second {
		return l.p
	}
	l.checked = now
	fi, err := os.Stat(filepath.Join(l.home, PricingFile))
	var mod time.Time
	var size int64 = -1
	if err == nil {
		mod, size = fi.ModTime(), fi.Size()
	}
	if l.loaded && mod.Equal(l.mod) && size == l.size {
		return l.p
	}
	p, _, err := LoadPricing(l.home)
	if err != nil && l.OnError != nil {
		l.OnError(err)
	}
	l.p, l.mod, l.size, l.loaded = p, mod, size, true
	return p
}
