package service_test

import (
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// recordingPurger captures the URLs it's asked to purge.
type recordingPurger struct {
	mu       sync.Mutex
	urls     [][]string
	failNext bool
}

func (r *recordingPurger) PurgeURLs(urls []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, append([]string(nil), urls...))
	if r.failNext {
		r.failNext = false
		return assertErr("purge failed")
	}
	return nil
}

func (r *recordingPurger) calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.urls))
	for i, u := range r.urls {
		out[i] = append([]string(nil), u...)
	}
	return out
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func TestManage_DeletePurgesURL(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	manage.Cache = purger
	manage.PublicURL = func(s domain.Slug) string { return "https://hostthis.dev/p/" + s.String() }

	owner := "key:test-id"
	res, err := upload.Create(htmlBody(200), owner, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := manage.Delete(res.Paste.Slug, owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	calls := purger.calls()
	if len(calls) != 1 {
		t.Fatalf("purger called %d times, want 1: %v", len(calls), calls)
	}
	wantURL := "https://hostthis.dev/p/" + res.Paste.Slug.String()
	if len(calls[0]) != 1 || calls[0][0] != wantURL {
		t.Errorf("purger urls: got %v, want [%s]", calls[0], wantURL)
	}
}

func TestManage_UpdatePurgesURL(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	manage.Cache = purger
	manage.PublicURL = func(s domain.Slug) string { return "https://hostthis.dev/p/" + s.String() }

	owner := "key:test-id"
	res, err := upload.Create(htmlBody(200), owner, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, err := manage.Update(res.Paste.Slug, owner, htmlBody(300), ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	calls := purger.calls()
	if len(calls) != 1 {
		t.Fatalf("purger called %d times, want 1: %v", len(calls), calls)
	}
}

func TestManage_DeletePurgeErrorDoesntFailOp(t *testing.T) {
	upload, manage, repo := newStack(t)
	purger := &recordingPurger{failNext: true}
	manage.Cache = purger
	manage.PublicURL = func(s domain.Slug) string { return "https://hostthis.dev/p/" + s.String() }

	owner := "key:test-id"
	res, err := upload.Create(htmlBody(200), owner, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := manage.Delete(res.Paste.Slug, owner); err != nil {
		t.Fatalf("delete returned %v despite purge failure — purge errors must be swallowed", err)
	}
	// And the row really is gone (delete actually succeeded, purge or no).
	if _, err := repo.Get(res.Paste.Slug); err == nil {
		t.Fatalf("paste should be deleted, but Get returned nil error")
	}
}

func TestManage_NoCacheConfiguredIsFine(t *testing.T) {
	// Cache and PublicURL left nil — the default. Delete must still work.
	upload, manage, _ := newStack(t)
	owner := "key:test-id"
	res, err := upload.Create(htmlBody(200), owner, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := manage.Delete(res.Paste.Slug, owner); err != nil {
		t.Fatalf("delete with no cache configured: %v", err)
	}
}
