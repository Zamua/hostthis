package domain

import (
	"context"
	"io"
)

// A site's small files share pack objects, so a deploy's object writes follow
// its bytes rather than its file count.
const (
	// PackMemberMaxBytes is the largest declared size that joins a pack.
	PackMemberMaxBytes = 1 << 20
	// PackTargetBytes is the stored length at which an open pack is written.
	PackTargetBytes = 8 << 20
)

// BlobRef locates stored bytes: a whole object, or a slice of a pack when
// Length is positive.
type BlobRef struct {
	Key    string
	Offset int64
	Length int64
}

// Packable reports whether a file of the declared size joins a pack. The
// declared size is only a routing hint: the quota guard meters actual bytes.
func Packable(declaredSize int64) bool {
	return declaredSize >= 0 && declaredSize <= PackMemberMaxBytes
}

// PackFull reports whether a pack of the given stored length is ready to write.
func PackFull(storedLen int64) bool {
	return storedLen >= PackTargetBytes
}

// BlobPack accumulates several files' at-rest bytes into one object, each
// member decodable on its own. Like FileSink, it is a port the byte plane
// implements.
type BlobPack interface {
	// Append encodes r as the next member and returns its stored span.
	Append(r io.Reader) (offset, length int64, err error)
	// AppendStored appends r, already one complete at-rest member, unchanged.
	AppendStored(r io.Reader) (offset, length int64, err error)
	// Len is the stored length so far.
	Len() int64
	// Commit writes the pack as one object.
	Commit(ctx context.Context) error
	// Close releases local state; it never deletes a committed object.
	Close() error
}
