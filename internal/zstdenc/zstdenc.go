// Package zstdenc is the at-rest blob encoder both byte paths share. Its pooled
// zstd encoders are sized for many concurrent uploads: one worker and a 1 MiB
// window each, so an in-flight upload costs about its window rather than the
// library default of several workers with 8 MiB windows, allocated fresh per
// stream.
package zstdenc

import (
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Magic opens every stored blob: 'H' 'Z' (hostthis-zstd), a reserved 0x00, and
// format version 1.
var Magic = [4]byte{'H', 'Z', 0x00, 0x01}

// Level is the at-rest compression level.
const Level = zstd.SpeedDefault

const windowSize = 1 << 20

var pool = sync.Pool{
	New: func() any {
		// Only invalid options make NewWriter fail, and these are constant.
		e, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(Level),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(windowSize),
		)
		if err != nil {
			panic(err)
		}
		return e
	},
}

func get(w io.Writer) *zstd.Encoder {
	e := pool.Get().(*zstd.Encoder)
	e.Reset(w)
	return e
}

func put(e *zstd.Encoder) {
	e.Reset(nil)
	pool.Put(e)
}

// Writer encodes into the at-rest format: Magic, then one zstd stream.
type Writer struct{ enc *zstd.Encoder }

// NewWriter writes Magic to w and returns a Writer encoding into it. The caller
// must Close it.
func NewWriter(w io.Writer) (*Writer, error) {
	if _, err := w.Write(Magic[:]); err != nil {
		return nil, err
	}
	return &Writer{enc: get(w)}, nil
}

func (w *Writer) Write(p []byte) (int, error) { return w.enc.Write(p) }

// Close ends the stream. Only a cleanly closed encoder returns to the pool, so
// no half-written state is reused.
func (w *Writer) Close() error {
	if err := w.enc.Close(); err != nil {
		return err
	}
	put(w.enc)
	return nil
}
