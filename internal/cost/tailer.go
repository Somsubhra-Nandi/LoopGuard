package cost

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/cespare/xxhash/v2"
)

// fingerprintLen is how much of a file's start identifies it. Transcript
// lines start with a session id and a timestamp, so two files never share
// their first kilobyte.
const fingerprintLen = 1024

// Offset is how far a transcript has been read: the byte after the last
// complete line, and a fingerprint of the file's first bytes
// (min(fingerprintLen, Offset) of them), so a file replaced by another one
// at the same path is noticed. It is stored in transcript_offsets (the
// fingerprint in the inode column: inode numbers aren't portable, and a
// fingerprint also catches a file rewritten in place).
type Offset struct {
	Offset      int64
	Fingerprint int64
}

// Tailer reads a growing JSONL file incrementally. It never loads the file
// whole: lines are read one by one from the stored offset, and only
// complete lines (ending in a newline) are consumed, so a line still being
// written is read on a later poll.
type Tailer struct {
	Path string
	At   Offset
}

// Poll reads the lines added since the last poll and calls fn with each
// (the slice is only valid during the call). If the file shrank below the
// offset (truncated) or its first bytes changed (replaced, rotated), it
// starts over from the beginning and reports reset; callers deduplicate by
// message id, so reading a line twice is harmless. A missing file is not an
// error (nothing to read yet).
func (t *Tailer) Poll(fn func(line []byte)) (reset bool, err error) {
	f, err := os.Open(t.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if t.At.Offset > 0 {
		fp, err := fingerprint(f, t.At.Offset)
		if fi.Size() < t.At.Offset || err != nil || fp != t.At.Fingerprint {
			t.At, reset = Offset{}, true
		}
	}
	if fi.Size() == t.At.Offset {
		return reset, nil
	}
	if _, err := f.Seek(t.At.Offset, io.SeekStart); err != nil {
		return reset, err
	}
	br := bufio.NewReaderSize(f, 256<<10)
	start := t.At.Offset
	off := start
	for {
		b, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// A line longer than the buffer: collect it whole.
			var buf bytes.Buffer
			buf.Write(b)
			for errors.Is(err, bufio.ErrBufferFull) {
				b, err = br.ReadSlice('\n')
				buf.Write(b)
			}
			b = buf.Bytes()
		}
		if err != nil {
			// EOF without a newline: a partial line, left for the next poll.
			if errors.Is(err, io.EOF) {
				break
			}
			return reset, err
		}
		off += int64(len(b))
		if len(bytes.TrimSpace(b)) > 0 {
			fn(b)
		}
	}
	if off != start || t.At.Offset == 0 {
		fp, err := fingerprint(f, off)
		if err != nil {
			return reset, err
		}
		t.At = Offset{Offset: off, Fingerprint: fp}
	}
	return reset, nil
}

// fingerprint hashes the first min(fingerprintLen, n) bytes of f.
func fingerprint(f *os.File, n int64) (int64, error) {
	n = min(n, fingerprintLen)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 0); err != nil && !(errors.Is(err, io.EOF) && n == 0) {
		return 0, err
	}
	return int64(xxhash.Sum64(buf)), nil
}
