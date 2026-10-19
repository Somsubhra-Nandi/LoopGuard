package session

// Ring is a fixed-capacity buffer that keeps the most recent items.
type Ring[T any] struct {
	buf   []T
	start int // index of the oldest item
	n     int
}

// NewRing returns an empty ring that holds up to capacity items.
func NewRing[T any](capacity int) *Ring[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring[T]{buf: make([]T, capacity)}
}

// Push appends v, evicting the oldest item when full.
func (r *Ring[T]) Push(v T) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = v
		r.n++
		return
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % len(r.buf)
}

// Len returns the number of items held.
func (r *Ring[T]) Len() int { return r.n }

// Cap returns the capacity.
func (r *Ring[T]) Cap() int { return len(r.buf) }

// At returns the i-th item, 0 being the oldest. It panics if i is out of
// range.
func (r *Ring[T]) At(i int) T {
	if i < 0 || i >= r.n {
		panic("session: ring index out of range")
	}
	return r.buf[(r.start+i)%len(r.buf)]
}

// Last returns the newest item and false if the ring is empty.
func (r *Ring[T]) Last() (T, bool) {
	if r.n == 0 {
		var zero T
		return zero, false
	}
	return r.At(r.n - 1), true
}

// Tail returns up to n of the newest items, oldest first, in a new slice.
func (r *Ring[T]) Tail(n int) []T {
	if n > r.n {
		n = r.n
	}
	if n < 0 {
		n = 0
	}
	out := make([]T, n)
	for i := 0; i < n; i++ {
		out[i] = r.At(r.n - n + i)
	}
	return out
}

// Reset empties the ring.
func (r *Ring[T]) Reset() {
	var zero T
	for i := range r.buf {
		r.buf[i] = zero
	}
	r.start, r.n = 0, 0
}
