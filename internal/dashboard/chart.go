package dashboard

import (
	"fmt"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
)

// chart is a bar chart of daily cost with the part inside loops
// highlighted, drawn as inline SVG on the server (no chart library, no
// script). Coordinates are preformatted so the HTML is stable.
type chart struct {
	Width, Height int
	PlotLeft      string // x of the plot area
	PlotRight     string
	Base          string // y of the baseline
	GridTop       string // y of the top grid line (the max)
	GridMid       string
	MaxLabel      string
	MidLabel      string
	Bars          []bar
	Labels        []axisLabel
	Empty         bool
	Total         string // the period's cost, and inside loops
	Loop          string
	Days          []report.Day // for the table behind the chart
}

type bar struct {
	X, W         string
	Y, H         string // the whole bar
	LoopY, LoopH string // the part inside loops (H "0": none)
	Title        string
}

type axisLabel struct {
	X, Text string
}

const (
	chartW, chartH         = 720, 220
	padL, padR, padT, padB = 56.0, 8.0, 12.0, 28.0
)

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }

// newChart lays out one bar per day.
func newChart(days []report.Day) chart {
	c := chart{Width: chartW, Height: chartH, PlotLeft: f1(padL), PlotRight: f1(chartW - padR), Days: days}
	base, top := chartH-padB, padT
	c.Base, c.GridTop, c.GridMid = f1(base), f1(top), f1((base+top)/2)
	var max, total, loop float64
	for _, d := range days {
		max = maxf(max, d.CostUSD)
		total += d.CostUSD
		loop += d.LoopCostUSD
	}
	c.Total, c.Loop = cost.FormatUSD(total), cost.FormatUSD(loop)
	if max <= 0 {
		c.Empty = true
		return c
	}
	scale := niceCeil(max)
	c.MaxLabel, c.MidLabel = shortUSD(scale), shortUSD(scale/2)
	slot := (chartW - padL - padR) / float64(len(days))
	w := slot * 0.72
	for i, d := range days {
		x := padL + float64(i)*slot + (slot-w)/2
		h := (base - top) * d.CostUSD / scale
		lh := (base - top) * d.LoopCostUSD / scale
		title := fmt.Sprintf("%s: %s API-equivalent", d.Date.Format("Mon Jan 2"), cost.FormatUSD(d.CostUSD))
		if d.LoopCostUSD > 0 {
			title += fmt.Sprintf(", %s inside loops (est.)", cost.FormatUSD(d.LoopCostUSD))
		}
		c.Bars = append(c.Bars, bar{X: f1(x), W: f1(w), Y: f1(base - h), H: f1(h), LoopY: f1(base - lh), LoopH: f1(lh), Title: title})
		if (len(days)-1-i)%7 == 0 {
			c.Labels = append(c.Labels, axisLabel{X: f1(x + w/2), Text: d.Date.Format("Jan 2")})
		}
	}
	return c
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// niceCeil rounds v up to 1, 2, 2.5, or 5 times a power of ten.
func niceCeil(v float64) float64 {
	p := 0.01
	for p*10 <= v {
		p *= 10
	}
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*p {
			return m * p
		}
	}
	return 10 * p
}

func shortUSD(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("$%.0f", v)
	case v >= 1:
		return fmt.Sprintf("$%.4g", v)
	}
	return fmt.Sprintf("$%.2f", v)
}
