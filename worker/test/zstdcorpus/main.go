// Command zstdcorpus writes the Worker decoder's golden corpus: blobs in
// hostthisd's at-rest format, encoded by internal/zstdenc, as <dir>/<name>.hz,
// and <dir>/manifest.json with each blob's decoded size and sha256. The
// decoded bytes are hashed as they are encoded, never written or held.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zamua/hostthis/internal/zstdenc"
)

type blob struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func write(dir, name string, n int64, src io.Reader) blob {
	f, err := os.Create(filepath.Join(dir, name+".hz"))
	must(err)
	bw := bufio.NewWriterSize(f, 1<<20)
	enc, err := zstdenc.NewWriter(bw)
	must(err)
	h := sha256.New()
	got, err := io.Copy(io.MultiWriter(enc, h), io.LimitReader(src, n))
	must(err)
	if got != n {
		panic(fmt.Sprintf("%s: wrote %d of %d", name, got, n))
	}
	must(enc.Close())
	must(bw.Flush())
	must(f.Close())
	return blob{n, hex.EncodeToString(h.Sum(nil))}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

var words = strings.Fields(`the a of to and in is for that with on as by this from be are it at or
an was not which have has but can will its their more one all new other some these than about
paste site worker stream decoder window block frame manifest slug upload read write blob
object bucket cache header request response memory compression index storage edge route`)

// markdown is markdown-like prose: compressible, but with entropy enough to
// fill many blocks.
type markdown struct {
	r   *rand.Rand
	buf []byte
}

func (m *markdown) Read(p []byte) (int, error) {
	for len(m.buf) < len(p) {
		w := func() string { return words[m.r.IntN(len(words))] }
		switch m.r.IntN(8) {
		case 0:
			m.buf = fmt.Appendf(m.buf, "\n## Section %d: %s %s\n\n", m.r.IntN(1000), w(), w())
		case 1:
			m.buf = fmt.Appendf(m.buf, "- item %d uses `%s_%d` and **%s**\n", m.r.IntN(100), w(), m.r.IntN(1<<16), w())
		default:
			for i := range 8 + m.r.IntN(24) {
				if i > 0 {
					m.buf = append(m.buf, ' ')
				}
				m.buf = append(m.buf, w()...)
			}
			m.buf = append(m.buf, ". "...)
		}
	}
	n := copy(p, m.buf)
	m.buf = m.buf[n:]
	return n, nil
}

type noise struct{ r *rand.ChaCha8 }

func (n noise) Read(p []byte) (int, error) { return n.r.Read(p) }

// repeat is one line over and over, as a log is.
type repeat struct {
	line []byte
	off  int
}

func (r *repeat) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.line[r.off]
		r.off = (r.off + 1) % len(r.line)
	}
	return len(p), nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: zstdcorpus <dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	must(os.MkdirAll(dir, 0o755))
	var seed [32]byte
	manifest := map[string]blob{
		"empty":          write(dir, "empty", 0, strings.NewReader("")),
		"onebyte":        write(dir, "onebyte", 1, strings.NewReader("x")),
		"text":           write(dir, "text", 3<<20, &markdown{r: rand.New(rand.NewPCG(1, 2))}),
		"incompressible": write(dir, "incompressible", 2<<20, noise{rand.NewChaCha8(seed)}),
		"repeat100m":     write(dir, "repeat100m", 100<<20, &repeat{line: []byte("2026-10-11T12:00:00Z INFO served slug=abc23456 status=200 bytes=1234\n")}),
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644))
}
