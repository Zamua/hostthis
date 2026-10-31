package service

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// stubSites satisfies SiteDeployer by embedding it, so a call the decorator
// should not touch nil-panics.
type stubSites struct {
	SiteDeployer
	err error
}

func (s stubSites) DeployToSlug(domain.Slug, io.Reader, domain.Owner) (SiteResult, error) {
	return SiteResult{}, s.err
}

func (s stubSites) Delete(domain.Slug, domain.Owner) error { return s.err }

// A redeploy or delete of a site or knowledge base purges unless its error
// proves nothing was written, the same rule as the paste verbs.
func TestSiteWritesPurgeUnlessNothingWasTouched(t *testing.T) {
	verbs := map[string]func(SiteDeployer, domain.Slug) error{
		"redeploy": func(d SiteDeployer, s domain.Slug) error {
			_, err := d.DeployToSlug(s, bytes.NewReader(nil), ownerOf(testOwner))
			return err
		},
		"delete": func(d SiteDeployer, s domain.Slug) error { return d.Delete(s, ownerOf(testOwner)) },
	}
	for name, call := range verbs {
		for _, tc := range []struct {
			name      string
			err       error
			wantPurge bool
		}{
			{"success", nil, true},
			{"transient storage error", assertErr("commit response lost"), true},
			{"wrapped transient error", fmt.Errorf("replace: %w", assertErr("timeout")), true},
			{"not found", ErrNotFound, false},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				purger := &recordingPurger{}
				d := NewSiteCacheInvalidating(stubSites{err: tc.err}, purger)
				if got := call(d, "abc12345"); got != tc.err {
					t.Fatalf("error passthrough: got %v, want %v", got, tc.err)
				}
				if gotPurge := len(purger.calls()) == 1; gotPurge != tc.wantPurge {
					t.Fatalf("purged=%v, want %v (err=%v)", gotPurge, tc.wantPurge, tc.err)
				}
			})
		}
	}
}

// Against the real deploy service: the owner's redeploy and delete purge the
// slug; a foreign redeploy or delete purges nothing.
func TestSiteCacheInvalidating_RealService(t *testing.T) {
	inner, _, _ := deployFixture(t)
	purger := &recordingPurger{}
	d := NewSiteCacheInvalidating(inner, purger)
	arc := func(body string) io.Reader {
		return bytes.NewReader(gzipTar(t, map[string]string{"index.html": body}))
	}
	res, err := d.Deploy(arc("<h1>v1</h1>"), ownerOf(testOwner))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	slug := res.Site.Slug
	purger.reset()

	if _, err := d.DeployToSlug(slug, arc("<h1>hijack</h1>"), ownerOf("key:someone-else")); err == nil {
		t.Fatal("want an error redeploying another owner's site")
	}
	if err := d.Delete(slug, ownerOf("key:someone-else")); err == nil {
		t.Fatal("want an error deleting another owner's site")
	}
	if got := purger.calls(); len(got) != 0 {
		t.Fatalf("foreign writes purged %v", got)
	}

	if _, err := d.DeployToSlug(slug, arc("<h1>v2</h1>"), ownerOf(testOwner)); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if got := purger.calls(); len(got) != 1 || got[0] != slug {
		t.Fatalf("redeploy purge: got %v, want [%s]", got, slug)
	}
	purger.reset()
	if err := d.Delete(slug, ownerOf(testOwner)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := purger.calls(); len(got) != 1 || got[0] != slug {
		t.Fatalf("delete purge: got %v, want [%s]", got, slug)
	}
}

// An unconfigured CDN (nil purger) still serves every mutation.
func TestSiteCacheInvalidating_NilPurgerServesMutations(t *testing.T) {
	inner, _, _ := deployFixture(t)
	d := NewSiteCacheInvalidating(inner, nil)
	owner := ownerOf("key:nil-purger")
	res, err := d.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>v1</h1>"})), owner)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := d.DeployToSlug(res.Site.Slug, bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>v2</h1>"})), owner); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if err := d.Delete(res.Site.Slug, owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
