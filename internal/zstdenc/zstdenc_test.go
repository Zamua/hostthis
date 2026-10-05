package zstdenc

import (
	"bytes"
	"crypto/rand"
	"io"
	"runtime"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func encode(t testing.TB, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	e := Get(&out)
	if _, err := io.Copy(e, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	Put(e)
	return out.Bytes()
}

func decode(t testing.TB, enc []byte) []byte {
	t.Helper()
	d, err := zstd.NewReader(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRoundTripAcrossReuse pins that a reused encoder carries no state between
// streams.
func TestRoundTripAcrossReuse(t *testing.T) {
	random := make([]byte, 3<<20)
	_, _ = rand.Read(random)
	bodies := [][]byte{{}, []byte("x"), bytes.Repeat([]byte("hostthis "), 400_000), random}
	for round := range 5 {
		for i, b := range bodies {
			if got := decode(t, encode(t, b)); !bytes.Equal(got, b) {
				t.Fatalf("round %d body %d: round trip mismatch", round, i)
			}
		}
	}
}

// TestConcurrentStreams pins that no encoder is shared between live streams.
// Run with -race.
func TestConcurrentStreams(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			body := bytes.Repeat([]byte{byte(i)}, 200_000+i)
			if got := decode(t, encode(t, body)); !bytes.Equal(got, body) {
				t.Errorf("stream %d mismatch", i)
			}
		})
	}
	wg.Wait()
}

// TestWarmEncodeAllocatesLittle pins that a warm encode of a 9 MiB body
// allocates a small fraction of the body, so concurrent uploads do not each
// cost tens of MiB.
func TestWarmEncodeAllocatesLittle(t *testing.T) {
	body := make([]byte, 9<<20)
	_, _ = rand.Read(body)
	run := func() {
		e := Get(io.Discard)
		if _, err := io.Copy(e, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		Put(e)
	}
	run()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 2<<20 {
		t.Fatalf("warm 9 MiB encode allocated %.1f MiB, want under 2 MiB", float64(got)/(1<<20))
	}
}
