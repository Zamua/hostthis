package service

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

// The streaming upload pipeline frames and encodes blobs itself instead of
// calling the storage encoder, so the two must stay byte-identical: a diverged
// magic prefix makes the read path treat fresh pastes as uncompressed, and a
// diverged encoder level changes the at-rest bytes the quota is charged for.
func TestStreamUploadMatchesStorageAtRestFormat(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"short", []byte("hello hostthis")},
		{"repetitive", bytes.Repeat([]byte("<p>hostthis</p>\n"), 8192)},
		// Mixed redundancy: the encoder level changes the output bytes here,
		// which uniform or high-entropy input does not reveal.
		{"mixed_redundancy", mixedRedundancyBytes(40000)},
		{"incompressible", pseudoRandomBytes(256 * 1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			staged, err := streamUpload(bytes.NewReader(tc.raw))
			if err != nil {
				t.Fatalf("streamUpload: %v", err)
			}
			want, err := storage.EncodeCompressedBody(bytes.NewReader(tc.raw))
			if err != nil {
				t.Fatalf("EncodeCompressedBody: %v", err)
			}
			if !bytes.Equal(stagedBytes(t, staged), want) {
				t.Fatalf("at-rest body differs between the streaming upload path and the storage encoder:\n stream: %d bytes, prefix %x\nstorage: %d bytes, prefix %x",
					len(stagedBytes(t, staged)), head(stagedBytes(t, staged), 8), len(want), head(want, 8))
			}
			if got, exp := staged.CompressedSize, len(want)-storage.CompressedBodyPrefixLen; got != exp {
				t.Fatalf("CompressedSize = %d, want %d (storage body minus framing prefix)", got, exp)
			}
			if got, exp := staged.RawSize, len(tc.raw); got != exp {
				t.Fatalf("RawSize = %d, want %d", got, exp)
			}
		})
	}
}

func head(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

// pseudoRandomBytes builds deterministic high-entropy input the encoder cannot
// shrink, exercising the case where framing overhead dominates.
func pseudoRandomBytes(n int) []byte {
	b := make([]byte, n)
	x := uint32(0x9e3779b9)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

// mixedRedundancyBytes builds deterministic text with a repeated vocabulary but
// varying structure, the shape whose encoding differs between zstd levels.
func mixedRedundancyBytes(words int) []byte {
	vocab := []string{"hostthis", "paste", "blob", "zstd", "storage", "service", "upload", "magic", "prefix", "compress"}
	var buf bytes.Buffer
	x := uint32(12345)
	for i := range words {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		fmt.Fprintf(&buf, "%s-%d ", vocab[x%uint32(len(vocab))], x%997)
		if i%17 == 0 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// stagedBytes reads a staged upload's spill file, for tests that assert on the
// at-rest bytes. The body is no longer held in memory.
func stagedBytes(t *testing.T, s stagedUpload) []byte {
	t.Helper()
	if s.File == nil {
		return nil
	}
	if _, err := s.File.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind staged spill: %v", err)
	}
	b, err := io.ReadAll(s.File)
	if err != nil {
		t.Fatalf("read staged spill: %v", err)
	}
	return b
}
