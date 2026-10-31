package service

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// recordingPurger is a CachePurger standing in for a CDN adapter, capturing
// the slugs it is asked to purge.
type recordingPurger struct {
	mu       sync.Mutex
	slugs    []domain.Slug
	failNext bool
}

func (r *recordingPurger) PurgePaste(slug domain.Slug) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slugs = append(r.slugs, slug)
	if r.failNext {
		r.failNext = false
		return assertErr("purge failed")
	}
	return nil
}

func (r *recordingPurger) calls() []domain.Slug {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Slug(nil), r.slugs...)
}

func (r *recordingPurger) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slugs = nil
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

const testOwner = "key:test-id"

// newPaste uploads a fresh HTML paste.
func newPaste(t *testing.T, upload *Upload) domain.Slug {
	t.Helper()
	res, err := upload.Create(bytes.NewReader(htmlBody(200)), ownerOf(testOwner), "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return res.Paste.Slug
}

// The decorator fires exactly one purge, naming the mutated slug, after
// each successful mutating verb.
func TestCacheInvalidating_MutationsPurge(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := NewCacheInvalidating(manage, purger)

	t.Run("update", func(t *testing.T) {
		purger.reset()
		slug := newPaste(t, upload)
		if _, err := mgr.Update(slug, ownerOf(testOwner), bytes.NewReader(htmlBody(300)), ""); err != nil {
			t.Fatalf("update: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("update purge: got %v, want [%s]", got, slug)
		}
	})
	t.Run("pin then unpin", func(t *testing.T) {
		slug := newPaste(t, upload)
		if _, err := mgr.Update(slug, ownerOf(testOwner), bytes.NewReader(htmlBody(300)), ""); err != nil {
			t.Fatalf("update: %v", err)
		}
		purger.reset()
		if _, err := mgr.Pin(slug, ownerOf(testOwner), 1); err != nil {
			t.Fatalf("pin: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("pin purge: got %v, want [%s]", got, slug)
		}
		purger.reset()
		if err := mgr.Unpin(slug, ownerOf(testOwner)); err != nil {
			t.Fatalf("unpin: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("unpin purge: got %v, want [%s]", got, slug)
		}
	})
}

// rename never changes the bytes served at the public URL, so it must NOT
// purge.
func TestCacheInvalidating_RenameDoesNotPurge(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := NewCacheInvalidating(manage, purger)

	slug := newPaste(t, upload)
	if err := mgr.Rename(slug, ownerOf(testOwner), "a-label"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := purger.calls(); len(got) != 0 {
		t.Fatalf("rename must not purge, got %v", got)
	}
}

// A purge is best-effort: its error is swallowed so the mutation still reports
// the success it actually achieved on origin.
func TestCacheInvalidating_PurgeErrorDoesNotFailOp(t *testing.T) {
	upload, manage, repo := newStack(t)
	purger := &recordingPurger{failNext: true}
	mgr := NewCacheInvalidating(manage, purger)

	slug := newPaste(t, upload)
	if err := mgr.Delete(slug, ownerOf(testOwner)); err != nil {
		t.Fatalf("delete returned %v despite purge failure - purge errors must be swallowed", err)
	}
	if _, err := repo.Get(slug); err == nil {
		t.Fatalf("paste should be deleted, but Get returned nil error")
	}
}

// An unconfigured CDN (nil purger) still serves every mutation.
func TestCacheInvalidating_NilPurgerServesMutations(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := ownerOf("key:nil-purger")
	res, err := upload.Create(strings.NewReader("# doc\n"), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := NewCacheInvalidating(manage, nil)
	if err := m.Rename(res.Paste.Slug, owner, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := m.Delete(res.Paste.Slug, owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// stubManager satisfies PasteManager by embedding it, so only the one method
// under test needs a body. Any other call would nil-panic, which is the point:
// it proves the decorator touched nothing else.
type stubManager struct {
	PasteManager
	err error
}

func (s stubManager) Delete(domain.Slug, domain.Owner) error { return s.err }

func (s stubManager) Update(domain.Slug, domain.Owner, io.Reader, string) (UpdateResult, error) {
	return UpdateResult{}, s.err
}

func (s stubManager) Pin(domain.Slug, domain.Owner, int) (domain.Version, error) {
	return domain.Version{}, s.err
}

func (s stubManager) Unpin(domain.Slug, domain.Owner) error { return s.err }

// Every mutating verb purges unless its error proves nothing was written: a
// transport error after the cell committed leaves the new state served.
func TestMutationsPurgeUnlessNothingWasTouched(t *testing.T) {
	verbs := map[string]func(PasteManager, domain.Slug) error{
		"update": func(m PasteManager, s domain.Slug) error {
			_, err := m.Update(s, ownerOf(testOwner), strings.NewReader("x"), "")
			return err
		},
		"pin": func(m PasteManager, s domain.Slug) error {
			_, err := m.Pin(s, ownerOf(testOwner), 1)
			return err
		},
		"unpin": func(m PasteManager, s domain.Slug) error { return m.Unpin(s, ownerOf(testOwner)) },
	}
	for name, call := range verbs {
		for _, tc := range []struct {
			name      string
			err       error
			wantPurge bool
		}{
			{"success", nil, true},
			{"transient storage error", assertErr("commit response lost"), true},
			{"wrapped transient error", fmt.Errorf("append: %w", assertErr("timeout")), true},
			{"not found", ErrNotFound, false},
			// Raised while staging the body, before ownership is checked.
			{"too large", ErrRawTooLarge, false},
			{"compressed too large", ErrCompressedTooLarge, false},
			{"empty upload", ErrEmptyUpload, false},
			{"staging", fmt.Errorf("%w: %w", errStaging, assertErr("disk full")), false},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				purger := &recordingPurger{}
				mgr := NewCacheInvalidating(stubManager{err: tc.err}, purger)
				if got := call(mgr, "abc12345"); got != tc.err {
					t.Fatalf("error passthrough: got %v, want %v", got, tc.err)
				}
				if gotPurge := len(purger.calls()) == 1; gotPurge != tc.wantPurge {
					t.Fatalf("purged=%v, want %v (err=%v)", gotPurge, tc.wantPurge, tc.err)
				}
			})
		}
	}
}

// An update of a slug the caller does not own never purges, whatever its body:
// the body is staged before ownership is checked, so its rejections must be
// recognised too.
func TestUpdateOfAnotherOwnersPasteDoesNotPurge(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := NewCacheInvalidating(manage, purger)
	slug := newPaste(t, upload)
	for name, body := range map[string]io.Reader{
		"html":      bytes.NewReader(htmlBody(300)),
		"empty":     strings.NewReader(""),
		"too large": io.LimitReader(zeros{}, int64(domain.HardRawByteCap)+1),
	} {
		t.Run(name, func(t *testing.T) {
			purger.reset()
			if _, err := mgr.Update(slug, ownerOf("key:someone-else"), body, ""); err == nil {
				t.Fatal("want an error updating another owner's paste")
			}
			if got := purger.calls(); len(got) != 0 {
				t.Fatalf("another owner's slug purged %v", got)
			}
		})
	}
}

// zeros is an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A delete purges unless the error proves the paste was never touched. The
// ambiguous middle - a removal that landed and then reported failure - is the
// case that leaves deleted content on the edge until max-age expires.
func TestDeletePurgesUnlessNothingWasTouched(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantPurge bool
	}{
		{"success", nil, true},
		{"transient storage error", assertErr("commit failed"), true},
		{"wrapped transient error", fmt.Errorf("delete: %w", assertErr("timeout")), true},

		// Pre-mutation rejections: no write was attempted, and purging here
		// would let any caller spend the CDN purge budget on slugs they do
		// not own.
		{"not found", ErrNotFound, false},
		{"wrapped not found", fmt.Errorf("check: %w", ErrNotFound), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			purger := &recordingPurger{}
			mgr := NewCacheInvalidating(stubManager{err: tc.err}, purger)

			if got := mgr.Delete(domain.Slug("abc12345"), ownerOf(testOwner)); got != tc.err {
				t.Fatalf("error passthrough: got %v, want %v", got, tc.err)
			}
			if gotPurge := len(purger.calls()) == 1; gotPurge != tc.wantPurge {
				t.Fatalf("purged=%v, want %v (err=%v)", gotPurge, tc.wantPurge, tc.err)
			}
		})
	}
}

// The stub test above proves the decorator's rule. This proves the rule matches
// the errors the REAL service actually produces, which is the half a stub
// cannot: if requireOwner ever stopped returning these sentinels, the exclusion
// would silently stop matching and unowned slugs would become purgeable.
func TestDeletePurgeExclusionsMatchRealServiceErrors(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := NewCacheInvalidating(manage, purger)

	t.Run("own paste purges", func(t *testing.T) {
		purger.reset()
		slug := newPaste(t, upload)
		if err := mgr.Delete(slug, ownerOf(testOwner)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("purge: got %v, want [%s]", got, slug)
		}
	})

	// Both of these must NOT purge, or attempting deletes becomes a way to
	// spend the CDN purge budget on slug you have no rights to.
	t.Run("absent slug does not purge", func(t *testing.T) {
		purger.reset()
		if err := mgr.Delete(domain.Slug("nosuchpp"), ownerOf(testOwner)); err == nil {
			t.Fatal("want an error deleting an absent slug")
		}
		if got := purger.calls(); len(got) != 0 {
			t.Fatalf("absent slug purged %v; the exclusion no longer matches the real error", got)
		}
	})

	t.Run("another owner's paste does not purge", func(t *testing.T) {
		slug := newPaste(t, upload)
		purger.reset()
		if err := mgr.Delete(slug, ownerOf("key:someone-else")); err == nil {
			t.Fatal("want an error deleting another owner's paste")
		}
		if got := purger.calls(); len(got) != 0 {
			t.Fatalf("other owner's slug purged %v; the exclusion no longer matches the real error", got)
		}
	})

}
