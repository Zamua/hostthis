package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

const (
	screenTextFileMax  = 2 << 20
	screenTextTotalMax = 64 << 20
	screenBinaryHead   = 4 << 10
)

var screenTextExt = map[string]bool{
	"": true, ".html": true, ".htm": true, ".js": true, ".mjs": true, ".css": true,
	".json": true, ".svg": true, ".txt": true, ".md": true, ".xml": true,
}

// screenBundle collects what a site deploy sends for screening: text files in
// full up to a cap, and the head of every other file so its type stays
// recognizable. It spills to disk as a gzip tar.
type screenBundle struct {
	file *os.File
	gz   *gzip.Writer
	tw   *tar.Writer
	text int64
	// overflow is set once the text total passes its cap.
	overflow bool
	err      error
}

func newScreenBundle(dir string) (*screenBundle, error) {
	f, err := os.CreateTemp(dir, "hostthis-screen-*")
	if err != nil {
		return nil, fmt.Errorf("screen bundle: %w", err)
	}
	gz := gzip.NewWriter(f)
	return &screenBundle{file: f, gz: gz, tw: tar.NewWriter(gz)}, nil
}

// capture tees r into a buffer bounded for p's type; add records it once the
// sink has consumed the file.
func (b *screenBundle) capture(p string, r io.Reader) (io.Reader, func()) {
	limit := int64(screenBinaryHead)
	text := screenTextExt[strings.ToLower(path.Ext(p))]
	if text {
		limit = screenTextFileMax
	}
	buf := &bytes.Buffer{}
	return io.TeeReader(r, &boundedBuffer{buf: buf, left: limit}), func() { b.add(p, buf.Bytes(), text) }
}

func (b *screenBundle) add(p string, body []byte, text bool) {
	if b.err != nil || b.overflow {
		return
	}
	if text {
		b.text += int64(len(body))
		if b.text > screenTextTotalMax {
			b.overflow = true
			return
		}
	}
	if b.err = b.tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); b.err != nil {
		return
	}
	_, b.err = b.tw.Write(body)
}

// request closes the bundle and returns it as a screening request.
func (b *screenBundle) request() (domain.ScreenRequest, error) {
	if b.err == nil {
		b.err = b.tw.Close()
	}
	if b.err == nil {
		b.err = b.gz.Close()
	}
	if b.err == nil {
		_, b.err = b.file.Seek(0, io.SeekStart)
	}
	if b.err != nil {
		return domain.ScreenRequest{}, b.err
	}
	return domain.ScreenRequest{Shape: domain.ScreenArchive, Body: b.file}, nil
}

func (b *screenBundle) discard() {
	_ = b.file.Close()
	_ = os.Remove(b.file.Name())
}

// boundedBuffer keeps the first left bytes written and accepts the rest
// unrecorded, so the tee never fails the sink's read.
type boundedBuffer struct {
	buf  *bytes.Buffer
	left int64
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if keep := min(int64(len(p)), w.left); keep > 0 {
		w.buf.Write(p[:keep])
		w.left -= keep
	}
	return len(p), nil
}

// checkBundle screens a finished site bundle; text past the cap refuses.
func (g *ScreenGate) checkBundle(owner domain.Owner, b *screenBundle) error {
	if b.overflow {
		return &RefusedError{Rule: "too-large-to-screen"}
	}
	req, err := b.request()
	if err != nil {
		if g.Logger != nil {
			g.Logger.Printf("screen: bundle: %v", err)
		}
		return ErrScreenUnavailable
	}
	return g.check(owner, req)
}
