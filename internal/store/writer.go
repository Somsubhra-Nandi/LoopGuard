package store

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"
)

// Row is a record the Writer can persist: Session, Call, Signal, or
// Intervention.
type Row interface {
	write(ctx context.Context, tx *sql.Tx) error
}

// Writer defaults (LLD §9.2).
const (
	DefaultFlushEvery = 200 * time.Millisecond
	DefaultBatchSize  = 100
	DefaultQueueSize  = 4096
)

// WriterOptions configures a Writer. Zero values take the defaults.
type WriterOptions struct {
	FlushEvery time.Duration
	BatchSize  int
	QueueSize  int
	// OnError is called (on the writer goroutine) when rows fail to persist.
	OnError func(err error, rows int)
}

// Writer persists rows in batches on its own goroutine: a batch is written
// in one transaction every FlushEvery, or as soon as BatchSize rows are
// queued. Enqueue never blocks: when the queue is full the row is dropped
// and counted, since losing a row is better than slowing the agent.
//
// All methods are safe on a nil *Writer (memory-only mode), and do nothing.
type Writer struct {
	s     *Store
	opts  WriterOptions
	ch    chan Row
	flush chan chan struct{}
	done  chan struct{}

	mu     sync.RWMutex // guards closed against Enqueue racing Close
	closed bool

	dropped atomic.Uint64
	failed  atomic.Uint64
	written atomic.Uint64
}

// NewWriter starts a Writer for s.
func (s *Store) NewWriter(opts WriterOptions) *Writer {
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = DefaultFlushEvery
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	w := &Writer{
		s:     s,
		opts:  opts,
		ch:    make(chan Row, opts.QueueSize),
		flush: make(chan chan struct{}),
		done:  make(chan struct{}),
	}
	go w.loop()
	return w
}

// Enqueue queues a row without blocking. It reports false if the row was
// dropped (queue full, writer closed, or nil writer).
func (w *Writer) Enqueue(r Row) bool {
	if w == nil {
		return false
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return false
	}
	select {
	case w.ch <- r:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

// Flush writes everything queued so far and returns when it is committed.
func (w *Writer) Flush() {
	if w == nil {
		return
	}
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return
	}
	ack := make(chan struct{})
	select {
	case w.flush <- ack:
		w.mu.RUnlock()
		<-ack
	case <-w.done:
		w.mu.RUnlock()
	}
}

// Close writes the remaining rows and stops the writer.
func (w *Writer) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.mu.Unlock()
	<-w.done
}

// Stats returns the rows written, dropped (queue full), and lost to failed
// writes so far.
func (w *Writer) Stats() (written, dropped, failed uint64) {
	if w == nil {
		return 0, 0, 0
	}
	return w.written.Load(), w.dropped.Load(), w.failed.Load()
}

func (w *Writer) loop() {
	defer close(w.done)
	tick := time.NewTicker(w.opts.FlushEvery)
	defer tick.Stop()
	batch := make([]Row, 0, w.opts.BatchSize)
	add := func(r Row) {
		batch = append(batch, r)
		if len(batch) >= w.opts.BatchSize {
			w.write(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case r, ok := <-w.ch:
			if !ok {
				w.write(batch)
				return
			}
			add(r)
		case <-tick.C:
			w.write(batch)
			batch = batch[:0]
		case ack := <-w.flush:
		drain:
			for {
				select {
				case r, ok := <-w.ch:
					if !ok {
						break drain
					}
					add(r)
				default:
					break drain
				}
			}
			w.write(batch)
			batch = batch[:0]
			close(ack)
		}
	}
}

// write persists one batch in a transaction. A row that fails is counted
// and skipped; SQLite rolls back only the failing statement.
func (w *Writer) write(batch []Row) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := w.s.db.BeginTx(ctx, nil)
	if err != nil {
		w.fail(err, len(batch))
		return
	}
	n, failed := 0, 0
	var firstErr error
	for _, r := range batch {
		if err := r.write(ctx, tx); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		w.fail(err, n+failed)
		return
	}
	w.written.Add(uint64(n))
	if failed > 0 {
		w.fail(firstErr, failed)
	}
}

func (w *Writer) fail(err error, rows int) {
	w.failed.Add(uint64(rows))
	if w.opts.OnError != nil {
		w.opts.OnError(err, rows)
	}
}
