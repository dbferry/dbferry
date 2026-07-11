package pipeline

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
