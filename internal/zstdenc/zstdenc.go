// Package zstdenc hands out pooled zstd encoders sized for many concurrent
// uploads: one worker and a 1 MiB window each, so an in-flight upload costs
// about its window rather than the library default of several workers with
// 8 MiB windows, allocated fresh per stream.
package zstdenc

import (
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

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

// Get returns an encoder writing to w. Close it, then hand it back with Put.
func Get(w io.Writer) *zstd.Encoder {
	e := pool.Get().(*zstd.Encoder)
	e.Reset(w)
	return e
}

// Put returns a closed encoder to the pool. An encoder whose stream failed is
// dropped instead, so no half-written state is reused.
func Put(e *zstd.Encoder) {
	e.Reset(nil)
	pool.Put(e)
}
