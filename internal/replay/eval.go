package replay

import (
	"fmt"
	"io"
	"math"
	"sort"
)

// DetectorMetrics are the offline metrics of one detector (TDD §6.2).
type DetectorMetrics struct {
	Detector string
	// Labeled detections.
	TP, FP, Unlabeled int
	// Caught is the number of real loops this detector caught.
	Caught    int
	Precision float64 // TP / (TP + FP); NaN without labeled detections
	Recall    float64 // Caught / real loops; NaN without real loops
	// MeanDelay is the mean number of calls from a real loop's first call
	// to this detector's first signal on it; NaN if it caught none.
	MeanDelay float64
}

// Eval is the result of matching detected loops against labels.
type Eval struct {
	Detectors []DetectorMetrics
	RealLoops int // real-labeled detections (merged when they overlap) plus missed loops
	Caught    int // real loops caught by any detector
	Missed    int // missed-labeled loops still not caught
	Unlabeled int // detected loops with no label
}

// realLoop is a span of calls in one stream that a person said is a loop.
type realLoop struct {
	session, agent string
	from, to       int
	delay          map[string]int // detector → calls from loop start to its first signal
}

// Evaluate computes precision, recall, and detection delay per detector.
//
// Real loops are the detections labeled real (overlapping ones in the same
// stream merge into one loop) plus the loops labeled missed. A missed loop
// that a detection now overlaps counts as caught by that detector.
func Evaluate(loops []*Loop, labels []Label) Eval {
	metrics := map[string]*DetectorMetrics{}
	m := func(d string) *DetectorMetrics {
		if metrics[d] == nil {
			metrics[d] = &DetectorMetrics{Detector: d}
		}
		return metrics[d]
	}
	var ev Eval
	var real []*realLoop
	addReal := func(session, agent string, from, to int) *realLoop {
		for _, r := range real {
			if r.session == session && r.agent == agent && overlaps(r.from, r.to, from, to) {
				r.from, r.to = min(r.from, from), max(r.to, to)
				return r
			}
		}
		r := &realLoop{session: session, agent: agent, from: from, to: to, delay: map[string]int{}}
		real = append(real, r)
		return r
	}
	catch := func(r *realLoop, l *Loop) {
		d := max(l.FirstSignalCall-r.from, 0)
		if old, ok := r.delay[l.Detector]; !ok || d < old {
			r.delay[l.Detector] = d
		}
	}

	for _, l := range loops {
		lab, ok := LabelFor(labels, l)
		switch {
		case !ok:
			m(l.Detector).Unlabeled++
			ev.Unlabeled++
		case lab.Verdict == VerdictReal:
			m(l.Detector).TP++
			catch(addReal(l.SessionID, l.AgentID, l.FirstCall, l.LastCall), l)
		case lab.Verdict == VerdictFalsePositive:
			m(l.Detector).FP++
		}
	}
	for _, lab := range labels {
		if lab.Kind != "missed" {
			continue
		}
		var hits []*Loop
		for _, l := range loops {
			if sameStream(lab.SessionID, lab.AgentID, l) && overlaps(lab.FromCall, lab.ToCall, l.FirstCall, l.LastCall) {
				if fp, ok := LabelFor(labels, l); ok && fp.Verdict == VerdictFalsePositive {
					continue
				}
				hits = append(hits, l)
			}
		}
		session := lab.SessionID
		if len(hits) > 0 {
			session = hits[0].SessionID
		}
		r := addReal(session, lab.AgentID, lab.FromCall, lab.ToCall)
		for _, l := range hits {
			catch(r, l)
		}
	}

	ev.RealLoops = len(real)
	delays := map[string][]int{}
	for _, r := range real {
		if len(r.delay) == 0 {
			ev.Missed++
			continue
		}
		ev.Caught++
		for d, n := range r.delay {
			m(d).Caught++
			delays[d] = append(delays[d], n)
		}
	}
	for d, dm := range metrics {
		dm.Precision = ratio(dm.TP, dm.TP+dm.FP)
		dm.Recall = ratio(dm.Caught, ev.RealLoops)
		dm.MeanDelay = math.NaN()
		if ds := delays[d]; len(ds) > 0 {
			sum := 0
			for _, x := range ds {
				sum += x
			}
			dm.MeanDelay = float64(sum) / float64(len(ds))
		}
		ev.Detectors = append(ev.Detectors, *dm)
	}
	sort.Slice(ev.Detectors, func(i, j int) bool { return ev.Detectors[i].Detector < ev.Detectors[j].Detector })
	return ev
}

func ratio(a, b int) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}

// WriteText prints the evaluation as a table.
func (ev Eval) WriteText(w io.Writer) error {
	p := &printer{w: w}
	p.f("%-18s %5s %5s %9s %10s %8s %8s %11s\n", "detector", "TP", "FP", "unlabeled", "precision", "caught", "recall", "mean delay")
	for _, d := range ev.Detectors {
		p.f("%-18s %5d %5d %9d %10s %8d %8s %11s\n", d.Detector, d.TP, d.FP, d.Unlabeled,
			pct(d.Precision), d.Caught, pct(d.Recall), calls(d.MeanDelay))
	}
	overall := ratio(ev.Caught, ev.RealLoops)
	p.f("\nreal loops %d: caught %d, missed %d (recall %s); %d detections unlabeled\n",
		ev.RealLoops, ev.Caught, ev.Missed, pct(overall), ev.Unlabeled)
	return p.err
}

func pct(x float64) string {
	if math.IsNaN(x) {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*x)
}

func calls(x float64) string {
	if math.IsNaN(x) {
		return "-"
	}
	return fmt.Sprintf("%.1f calls", x)
}
