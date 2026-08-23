package service

// Pins that the owner-controlled read streams instead of buffering.
//
// The property is not "Show returns bytes" but "Show never materialises the
// whole document": it would otherwise drain the DECOMPRESSED stream, which can
// be an order of magnitude larger than the compressed cap, making this the
// largest allocation the service can be asked for (docs/SPEC.md "Reads are
// constant-memory too").
//
// The double FAILS ReadAll rather than counting allocations: a memory
// measurement would be flaky, while "the buffering method was never reached" is
// exactly the contract and cannot pass for the wrong reason.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

const showOwner = "key:show-stream"

// showRepoStub provides ONLY Get; the embedded nil PasteAdmin makes any other
// repo call panic, so a pass proves Show touches nothing else.
type showRepoStub struct {
	PasteAdmin
	owner string
}

func (s *showRepoStub) Get(slug domain.Slug) (domain.Paste, error) {
	return domain.Paste{Slug: slug, Identity: domain.Identity(s.owner), ContentSHA: "sha-1"}, nil
}

// streamOnlyBlobUnit serves Read and refuses ReadAll, so a caller that buffers
// fails loudly instead of silently allocating. The embedded nil BlobUnit makes
// every other blob call panic.
type streamOnlyBlobUnit struct {
	BlobUnit
	body  string
	reads int
}

func (u *streamOnlyBlobUnit) Read(context.Context, string, string) (io.ReadCloser, int64, error) {
	u.reads++
	return io.NopCloser(strings.NewReader(u.body)), int64(len(u.body)), nil
}

func (u *streamOnlyBlobUnit) ReadAll(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("ReadAll must not be reached: the read path must stream")
}

func TestShow_StreamsAndNeverBuffers(t *testing.T) {
	const body = "the whole document, which must never be materialised"
	blob := &streamOnlyBlobUnit{body: body}
	m := NewManage(&showRepoStub{owner: showOwner}, blob)

	_, rc, err := m.Show(domain.Slug("abcd1234"), showOwner)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer rc.Close() //nolint:errcheck

	if blob.reads != 1 {
		t.Fatalf("Read called %d times; want exactly 1 (Show must take the streaming path)", blob.reads)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("draining the returned stream: %v", err)
	}
	if string(got) != body {
		t.Fatalf("streamed %q; want %q", got, body)
	}
}

// Handing back an UNREAD stream is the point: Show must not consume it on the
// caller's behalf, or the allocation simply moved.
func TestShow_ReturnsAnUndrainedStream(t *testing.T) {
	blob := &streamOnlyBlobUnit{body: "abcdefghij"}
	m := NewManage(&showRepoStub{owner: showOwner}, blob)

	_, rc, err := m.Show(domain.Slug("abcd1234"), showOwner)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer rc.Close() //nolint:errcheck

	first := make([]byte, 3)
	if _, err := io.ReadFull(rc, first); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	if string(first) != "abc" {
		t.Fatalf("first 3 bytes = %q; want \"abc\" (the stream had already been consumed)", first)
	}
	rest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("draining the remainder: %v", err)
	}
	if string(rest) != "defghij" {
		t.Fatalf("remainder = %q; want \"defghij\"", rest)
	}
}

// Ownership gates the read, and the gate runs BEFORE any blob work. The error
// is ErrNotFound rather than ErrNotOwner by design: requireOwner refuses to
// leak the existence of another identity's slug.
func TestShow_RefusesNonOwnerWithoutTouchingTheBlob(t *testing.T) {
	blob := &streamOnlyBlobUnit{body: "secret"}
	m := NewManage(&showRepoStub{owner: showOwner}, blob)

	_, _, err := m.Show(domain.Slug("abcd1234"), "key:someone-else")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Show as a non-owner = %v; want ErrNotFound", err)
	}
	if blob.reads != 0 {
		t.Fatalf("Read called %d times for a refused request; want 0", blob.reads)
	}
}
