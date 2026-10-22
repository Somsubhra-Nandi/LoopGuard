package cost

import (
	"path/filepath"
	"sort"
)

// PricingStatus describes the price table in use, for doctor and the
// dashboard's settings page.
type PricingStatus struct {
	Path    string   `json:"path"`
	Exists  bool     `json:"exists"`
	Error   string   `json:"error,omitempty"` // invalid: the shipped table is used
	Models  int      `json:"models"`
	Checked []string `json:"checked"` // the distinct `checked` dates
	// Unverified are entries whose price nobody checked (cost unknown).
	Unverified []string `json:"unverified,omitempty"`
	// IDAssumed are entries whose model id was inferred, not seen
	// (id_verified: false); their prices are checked.
	IDAssumed []string `json:"id_assumed,omitempty"`
}

// Status loads the home's pricing.yaml and describes it.
func Status(home string) (*Pricing, PricingStatus) {
	p, exists, err := LoadPricing(home)
	s := PricingStatus{Path: filepath.Join(home, PricingFile), Exists: exists, Models: len(p.Models), Checked: []string{}}
	if err != nil {
		s.Error = err.Error()
	}
	checked := map[string]bool{}
	for _, m := range p.Models {
		switch {
		case !m.Verified():
			s.Unverified = append(s.Unverified, m.Match)
		default:
			checked[m.Checked] = true
		}
		if m.IDAssumed() {
			s.IDAssumed = append(s.IDAssumed, m.Match)
		}
	}
	for c := range checked {
		s.Checked = append(s.Checked, c)
	}
	sort.Strings(s.Checked)
	return p, s
}
