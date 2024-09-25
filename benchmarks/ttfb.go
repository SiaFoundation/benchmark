package benchmarks

import "time"

// A TTFBWriter is an io.Writer that records the time between initialization
// and when the first byte is written to it.
type TTFBWriter struct {
	start time.Time
	write time.Time
}

func (w *TTFBWriter) Write(p []byte) (int, error) {
	if w.write == (time.Time{}) {
		w.write = time.Now()
	}
	return len(p), nil
}

func (w *TTFBWriter) TTFB() time.Duration {
	return w.write.Sub(w.start)
}

func newTTFBWriter() *TTFBWriter {
	return &TTFBWriter{start: time.Now()}
}
