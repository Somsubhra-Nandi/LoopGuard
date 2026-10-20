package daemon

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// latencyBuckets are the event_latency histogram's upper bounds, in
// seconds: the hook budget is 800 ms, and the target p99 is 50 ms.
var latencyBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

// metrics are the daemon's Prometheus metrics, kept by hand: a few
// counters do not justify the client library.
type metrics struct {
	mu        sync.Mutex
	events    map[[2]string]uint64 // (agent, event) → count
	decisions map[string]uint64    // would_have → count
	errors    map[string]uint64    // kind → count
	buckets   []uint64             // per latencyBuckets, non-cumulative
	sum       float64
	count     uint64
}

func newMetrics() *metrics {
	return &metrics{
		events:    map[[2]string]uint64{},
		decisions: map[string]uint64{},
		errors:    map[string]uint64{},
		buckets:   make([]uint64, len(latencyBuckets)+1),
	}
}

func (m *metrics) event(agent, ev string) {
	m.mu.Lock()
	m.events[[2]string{agent, ev}]++
	m.mu.Unlock()
}

func (m *metrics) decision(wouldHave string) {
	m.mu.Lock()
	m.decisions[wouldHave]++
	m.mu.Unlock()
}

// error counts a failure kind: parse, timeout, queue_full, panic, ...
func (m *metrics) error(kind string) {
	m.mu.Lock()
	m.errors[kind]++
	m.mu.Unlock()
}

func (m *metrics) latency(d time.Duration) {
	s := d.Seconds()
	i := sort.SearchFloat64s(latencyBuckets, s)
	m.mu.Lock()
	m.buckets[i]++
	m.sum += s
	m.count++
	m.mu.Unlock()
}

// storeStats are the batched writer's counters.
type storeStats struct {
	written, dropped, failed uint64
	memoryOnly               bool
}

func (m *metrics) write(w io.Writer, activeSessions int, st storeStats) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintln(w, "# HELP loopguard_events_total Hook events received, by agent and event phase.")
	fmt.Fprintln(w, "# TYPE loopguard_events_total counter")
	keys := make([][2]string, 0, len(m.events))
	for k := range m.events {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(w, "loopguard_events_total{agent=%q,event=%q} %d\n", k[0], k[1], m.events[k])
	}

	fmt.Fprintln(w, "# HELP loopguard_decisions_total Decisions made, by the action enforce mode would have taken.")
	fmt.Fprintln(w, "# TYPE loopguard_decisions_total counter")
	writeCounters(w, "loopguard_decisions_total", "would_have", m.decisions)

	fmt.Fprintln(w, "# HELP loopguard_errors_total Events answered with no decision because of an error, by kind.")
	fmt.Fprintln(w, "# TYPE loopguard_errors_total counter")
	writeCounters(w, "loopguard_errors_total", "kind", m.errors)

	fmt.Fprintln(w, "# HELP loopguard_event_latency_seconds Time from receiving an event to replying.")
	fmt.Fprintln(w, "# TYPE loopguard_event_latency_seconds histogram")
	var cum uint64
	for i, le := range latencyBuckets {
		cum += m.buckets[i]
		fmt.Fprintf(w, "loopguard_event_latency_seconds_bucket{le=\"%g\"} %d\n", le, cum)
	}
	fmt.Fprintf(w, "loopguard_event_latency_seconds_bucket{le=\"+Inf\"} %d\n", m.count)
	fmt.Fprintf(w, "loopguard_event_latency_seconds_sum %g\n", m.sum)
	fmt.Fprintf(w, "loopguard_event_latency_seconds_count %d\n", m.count)

	fmt.Fprintln(w, "# HELP loopguard_active_sessions Sessions with a live actor.")
	fmt.Fprintln(w, "# TYPE loopguard_active_sessions gauge")
	fmt.Fprintf(w, "loopguard_active_sessions %d\n", activeSessions)

	fmt.Fprintln(w, "# HELP loopguard_store_rows_total Rows handed to the store, by outcome.")
	fmt.Fprintln(w, "# TYPE loopguard_store_rows_total counter")
	fmt.Fprintf(w, "loopguard_store_rows_total{outcome=\"written\"} %d\n", st.written)
	fmt.Fprintf(w, "loopguard_store_rows_total{outcome=\"dropped\"} %d\n", st.dropped)
	fmt.Fprintf(w, "loopguard_store_rows_total{outcome=\"failed\"} %d\n", st.failed)
	fmt.Fprintln(w, "# HELP loopguard_store_memory_only 1 if the database could not be opened and nothing is persisted.")
	fmt.Fprintln(w, "# TYPE loopguard_store_memory_only gauge")
	mo := 0
	if st.memoryOnly {
		mo = 1
	}
	fmt.Fprintf(w, "loopguard_store_memory_only %d\n", mo)
}

func writeCounters(w io.Writer, name, label string, m map[string]uint64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, m[k])
	}
}
