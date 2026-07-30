// Package archive is the gzip-tar ADAPTER for site uploads.
//
// It owns the codec and nothing else: open the gzip stream, iterate the tar,
// map tar's type flags onto the domain's format-neutral entry kinds, and hand
// each entry to the domain extractor. Every security guard - path traversal,
// decompression bomb, file and entry counts, manifest size - lives in the
// domain, because none of them is about tar. They would read identically for a
// zip or a directory walk.
//
// This package exists so the domain does not import archive/tar and
// compress/gzip. It previously did, which no layering rule caught: neither is a
// forbidden package by name, but both are wire formats, and a wire format is a
// mechanism the domain has no business knowing.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/Zamua/hostthis/internal/domain"
)

// Untar streams a gzip-tar archive from src, applying the domain's site-upload
// guards to every entry, and returns the resulting manifest.
//
// quotaBudget is the identity's REMAINING quota in bytes; the domain caps the
// site at min(quotaBudget, MaxSiteBytes).
//
// Performs NO durable I/O of its own: it reads src and calls sink.Store. On
// error the caller treats the deploy as failed and persists nothing - blobs the
// sink may already have written are content-addressed and get GC'd if they end
// up unreferenced.
func Untar(src io.Reader, sink domain.FileSink, quotaBudget int64) (domain.Manifest, error) {
	ex := domain.NewSiteExtractor(quotaBudget)

	gz, err := gzip.NewReader(src)
	if err != nil {
		// Not a valid gzip stream after all (truncated / corrupt). An
		// unsupported upload, not a server error.
		return domain.Manifest{}, fmt.Errorf("%w: not a valid gzip stream: %v", domain.ErrUnsupportedKind, err)
	}
	defer gz.Close() //nolint:errcheck

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return domain.Manifest{}, fmt.Errorf("%w: corrupt tar: %v", domain.ErrUnsupportedKind, err)
		}
		// tr is the reader for the CURRENT entry and is only valid until the
		// next Next(), which is exactly the lifetime Add needs.
		if err := ex.Add(entryOf(hdr), tr, sink); err != nil {
			return domain.Manifest{}, err
		}
	}
	return ex.Finish(), nil
}

// legacyTypeRegA is tar's pre-1.11 alias for a regular file. Go's reader
// normalises it to TypeReg, so this arm is belt-and-braces rather than
// load-bearing - spelled as the raw byte because the named constant is
// deprecated and staticcheck rejects it.
const legacyTypeRegA = '\x00'

// entryOf maps a tar header onto the domain's format-neutral entry.
//
// The mapping is deliberately CLOSED: anything that is not a plain file or a
// directory becomes EntryOther, which the domain rejects. Listing the allowed
// types rather than the disallowed ones is what keeps an exotic or
// newly-standardised type flag from defaulting into "allowed" - that category
// is where the link-following and device-node attacks live.
func entryOf(hdr *tar.Header) domain.ArchiveEntry {
	kind := domain.EntryOther
	switch hdr.Typeflag {
	case tar.TypeDir:
		kind = domain.EntryDir
	case tar.TypeReg, legacyTypeRegA:
		kind = domain.EntryFile
	}
	return domain.ArchiveEntry{
		Name:     hdr.Name,
		Size:     hdr.Size,
		Kind:     kind,
		TypeCode: int(hdr.Typeflag),
	}
}
