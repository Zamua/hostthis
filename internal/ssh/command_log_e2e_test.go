package ssh_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestEverySessionLogsItsCommandWithoutPayloadOrLabel(t *testing.T) {
	logged := &syncBuffer{}
	s := startStack(t, withLogOutput(logged))
	out, _, code := s.run("--name secretlabel", []byte("<!doctype html><p>private body</p>"))
	if code != 0 {
		t.Fatalf("upload exit %d", code)
	}
	slug := extractSlug(out)
	if _, _, code := s.run("versions "+slug, nil); code != 0 {
		t.Fatalf("versions exit %d", code)
	}

	want := "ssh: command verb=versions slug=" + slug + " outcome=ok"
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logged.String(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := logged.String()
	if !strings.Contains(got, want) || !strings.Contains(got, "ssh: command verb=upload slug=- outcome=ok") {
		t.Fatalf("log = %q, want an upload line and %q", got, want)
	}
	if strings.Contains(got, "secretlabel") || strings.Contains(got, "private body") {
		t.Fatalf("log = %q, carries a label or payload", got)
	}
}
