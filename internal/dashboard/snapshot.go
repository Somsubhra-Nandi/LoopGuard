package dashboard

import (
	"context"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/report"
)

// The overview's totals and the default session list read 30 days of
// usage (tens of thousands of rows with hundreds of sessions), which
// takes longer than a page should. They are computed into a snapshot
// that pages share: under snapshotFresh old it is served as is; up to
// snapshotStale old it is served while a new one is computed in the
// background; older (or none), the page waits for a new one. A login link
// computes one ahead, so the first page is quick. Live state (levels,
// live sessions, pauses, budgets) is never cached.
const (
	snapshotFresh   = 15 * time.Second
	snapshotStale   = 2 * time.Minute
	snapshotTimeout = 60 * time.Second
)

type snapshot struct {
	at       time.Time // when it was computed
	summary  report.Summary
	days     []report.Day
	from     time.Time // the default session list's first day
	sessions []report.SessionRow
	err      error // listing the sessions
}

// snapshot returns a snapshot recent enough for a page (see above).
func (d *Dashboard) snapshot(ctx context.Context) *snapshot {
	d.snapMu.Lock()
	cur := d.snap
	var age time.Duration
	if cur != nil {
		age = d.o.Now().Sub(cur.at)
	}
	switch {
	case cur != nil && age >= 0 && age < snapshotFresh:
		d.snapMu.Unlock()
		return cur
	case cur != nil && age >= 0 && age < snapshotStale:
		d.refreshLocked()
		d.snapMu.Unlock()
		return cur
	}
	done := d.refreshLocked()
	d.snapMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
	d.snapMu.Lock()
	defer d.snapMu.Unlock()
	if d.snap == nil {
		return &snapshot{at: d.o.Now(), summary: report.Summary{Errors: []string{"the totals are still being computed; reload in a moment"}},
			sessions: []report.SessionRow{}}
	}
	return d.snap
}

// prewarm computes a snapshot ahead unless a fresh one exists.
func (d *Dashboard) prewarm() {
	d.snapMu.Lock()
	defer d.snapMu.Unlock()
	if d.snap == nil || d.o.Now().Sub(d.snap.at) >= snapshotFresh {
		d.refreshLocked()
	}
}

// refreshLocked starts computing a snapshot unless one is being computed,
// and returns a channel closed when it is ready. d.snapMu is held.
func (d *Dashboard) refreshLocked() chan struct{} {
	if d.refreshing != nil {
		return d.refreshing
	}
	ch := make(chan struct{})
	d.refreshing = ch
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
		s := d.computeSnapshot(ctx)
		cancel()
		d.snapMu.Lock()
		d.snap, d.refreshing = s, nil
		d.snapMu.Unlock()
		close(ch)
	}()
	return ch
}

func (d *Dashboard) computeSnapshot(ctx context.Context) *snapshot {
	if d.o.Backend != nil {
		d.o.Backend.Flush()
	}
	at := d.now()
	p := d.pricing()
	nudgeAt := report.NudgeAt()
	s := &snapshot{at: d.o.Now(), from: report.Midnight(at).AddDate(0, 0, -(chartDays - 1)), sessions: []report.SessionRow{}}
	s.summary, s.days = report.Overview(ctx, d.o.Store, p, at, chartDays, nudgeAt)
	if d.o.Store != nil {
		s.sessions, s.err = report.ListSessions(ctx, d.o.Store, p, report.SessionFilter{Since: s.from, From: s.from, Limit: sessionLimit + 1}, nudgeAt)
	}
	return s
}

// asOf says how old a snapshot is.
func (d *Dashboard) asOf(s *snapshot) string {
	return "Updated " + report.ShortDuration(d.o.Now().Sub(s.at)) + " ago"
}
