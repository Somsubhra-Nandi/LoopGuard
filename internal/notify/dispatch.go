package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
)

// Rate limits (LLD §15), per channel: a phone that gets every loop's
// halt but also every loop's nudge would soon be muted.
const (
	PerLoopEvery = 10 * time.Minute // at most one notification per loop
	PerHour      = 5                // at most this many per hour overall
	queueSize    = 16
)

// Backoff is the wait before each retry of a failed network send (ntfy,
// webhook): 3 retries, exponential. Desktop sends aren't retried.
var Backoff = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

// Dispatcher sends notifications off the hook's path: Dispatch only
// checks the rate limits and queues, one queue and goroutine per channel,
// so a slow webhook never delays a hook reply or another channel. A full
// queue drops the notification (logged). Safe for concurrent use.
type Dispatcher struct {
	senders Senders
	log     *slog.Logger
	now     func() time.Time
	backoff []time.Duration

	mu     sync.Mutex
	limits map[string]*limiter      // by channel
	queues map[string]chan delivery // by channel, started on first use
	closed bool
	wg     sync.WaitGroup
	done   chan struct{}
}

type delivery struct {
	ch config.Channel
	n  Notification
}

// NewDispatcher returns a dispatcher that sends with senders.
func NewDispatcher(senders Senders, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{senders: senders, log: log, now: time.Now, backoff: Backoff,
		limits: map[string]*limiter{}, queues: map[string]chan delivery{}, done: make(chan struct{})}
}

// Dispatch queues n on every asynchronous channel (desktop, ntfy, webhook)
// that is enabled for its event in cfg and within its rate limits. It
// never blocks. It returns the channels n was queued on.
func (d *Dispatcher) Dispatch(cfg config.Notify, n Notification) []string {
	var sent []string
	for _, name := range []string{config.ChannelDesktop, config.ChannelNtfy, config.ChannelWebhook} {
		ch := cfg.Channels()[name]
		if !cfg.Wants(ch, n.Event) || d.senders[name] == nil || !d.allow(name, n) {
			continue
		}
		if d.enqueue(name, delivery{ch, n}) {
			sent = append(sent, name)
		}
	}
	return sent
}

// Terminal returns the text of a terminal notification for n, or "" if
// the terminal channel doesn't want it (off, not its event, rate limited,
// or the session isn't interactive). The text goes out with the hook
// reply (terminalSequence), so this never waits on anything.
func (d *Dispatcher) Terminal(cfg config.Notify, n Notification, interactive bool) string {
	if !interactive || !cfg.Wants(cfg.Terminal, n.Event) || !d.allow(config.ChannelTerminal, n) {
		return ""
	}
	return n.Text()
}

// allow applies channel's rate limits to n and, if it may go, counts it.
func (d *Dispatcher) allow(channel string, n Notification) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.limits[channel]
	if l == nil {
		l = &limiter{last: map[string]time.Time{}}
		d.limits[channel] = l
	}
	return l.allow(n, d.now())
}

func (d *Dispatcher) enqueue(name string, dl delivery) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	q := d.queues[name]
	if q == nil {
		q = make(chan delivery, queueSize)
		d.queues[name] = q
		d.wg.Add(1)
		go d.work(name, q)
	}
	select {
	case q <- dl:
		return true
	default:
		d.log.Warn("notification dropped: queue full", "channel", name, "event", dl.n.Event)
		return false
	}
}

// work sends one channel's notifications in order, retrying network
// channels with backoff.
func (d *Dispatcher) work(name string, q chan delivery) {
	defer d.wg.Done()
	s := d.senders[name]
	for dl := range q {
		attempts := 1
		if name != config.ChannelDesktop {
			attempts += len(d.backoff)
		}
		for i := 0; i < attempts; i++ {
			if i > 0 {
				select {
				case <-time.After(d.backoff[i-1]):
				case <-d.done:
					attempts = 0 // shutting down: give up
				}
				if attempts == 0 {
					break
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			err := s.Send(ctx, dl.ch, dl.n)
			cancel()
			if err == nil {
				d.log.Info("notification sent", "channel", name, "event", dl.n.Event, "session", dl.n.Session)
				break
			}
			d.log.Warn("notification failed", "channel", name, "event", dl.n.Event, "attempt", i+1, "err", err)
		}
	}
}

// Close stops the workers after the queued notifications, waiting at most
// until ctx is done; retries still waiting are abandoned.
func (d *Dispatcher) Close(ctx context.Context) {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.done)
		for _, q := range d.queues {
			close(q)
		}
	}
	d.mu.Unlock()
	finished := make(chan struct{})
	go func() { d.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
	}
}

// limiter is one channel's rate limit state.
type limiter struct {
	last   map[string]time.Time // loop → last notification
	recent []time.Time          // notifications in the last hour
}

// allow: at most one notification per loop per PerLoopEvery, except a
// halt (a loop's last word: the human has to act), and at most PerHour per
// hour. Tests are never limited.
func (l *limiter) allow(n Notification, now time.Time) bool {
	if n.Event == EventTest {
		return true
	}
	keep := l.recent[:0]
	for _, t := range l.recent {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	l.recent = keep
	if len(l.recent) >= PerHour {
		return false
	}
	loop := n.SessionKey + "\x00" + n.LoopKey
	if t, ok := l.last[loop]; ok && now.Sub(t) < PerLoopEvery && n.Event != config.EventHalt {
		return false
	}
	for k, t := range l.last {
		if now.Sub(t) >= PerLoopEvery {
			delete(l.last, k)
		}
	}
	l.last[loop] = now
	l.recent = append(l.recent, now)
	return true
}
