package pipeline

import (
	"io"
	"sync/atomic"
)

// cappedBuffer is an io.Writer that retains only the last max bytes written to
// it. It captures the tail of a child process's stderr for error messages
// without letting a chatty tool grow memory without bound.
type cappedBuffer struct {
	max int
	buf []byte
}

func newCappedBuffer(max int) *cappedBuffer {
	return &cappedBuffer{max: max}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	return string(b.buf)
}

// countReader tallies bytes read through it into an atomic counter, for live
// progress reporting without disturbing the stream.
type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}
