package service

// Pins the pre-claim compensation contract: a transactional deploy that stakes
// a slug and then fails must hand the slug back, and a deploy that commits must
// keep it. A leaked claim removes the slug from the site namespace for good,
// since PreClaimSlug rejects any slug that still carries one.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// claimSiteRepo records the pre-claim / release pairs the deploy path drives
// and can fail the authoritative insert on demand.
type claimSiteRepo struct {
	claimed   []domain.Slug
	released  []domain.Slug
	insertErr error
}

func (r *claimSiteRepo) InsertWithQuotaCheck(_ context.Context, _ domain.Site, _ int, _ int64, _ time.Time) error {
	return r.insertErr
}

func (r *claimSiteRepo) ReplaceWithQuotaCheck(_ context.Context, _ domain.Site, _ int, _ int64, _ time.Time) error {
	return errors.New("claimSiteRepo: ReplaceWithQuotaCheck unused")
}

func (r *claimSiteRepo) Get(domain.Slug) (domain.Site, error) {
	return domain.Site{}, domain.ErrNotFound
}

func (r *claimSiteRepo) Delete(domain.Slug) error { return nil }

func (r *claimSiteRepo) SumActiveBytesByOwner(string, time.Time) (int64, error) { return 0, nil }

func (r *claimSiteRepo) ListSitesByOwner(string, time.Time) ([]domain.Site, error) { return nil, nil }

func (r *claimSiteRepo) PreClaimSlug(_ context.Context, slug domain.Slug, _ string, _ time.Time) error {
	r.claimed = append(r.claimed, slug)
	return nil
}

func (r *claimSiteRepo) ReleaseSlugClaim(_ context.Context, slug domain.Slug, _ string) error {
	r.released = append(r.released, slug)
	return nil
}

var (
	_ SiteRepo          = (*claimSiteRepo)(nil)
	_ SlugClaimReleaser = (*claimSiteRepo)(nil)
	_ PasteByteSummer   = zeroPasteBytes{}
	_ BlobUnit          = txTestBlobUnit{}
)

type zeroPasteBytes struct{}

func (zeroPasteBytes) SumActiveBytesByOwner(string, time.Time) (int, error) { return 0, nil }

// txTestBlobUnit is a transactional BlobUnit that stages to nowhere, so the
// deploy takes the pre-claim branch without a real blob plane.
type txTestBlobUnit struct{}

func (txTestBlobUnit) Stage(context.Context, string, string, []byte) (BlobHandle, error) {
	return BlobHandle{}, nil
}

func (txTestBlobUnit) StageEncoding(_ context.Context, slug, sha string, r io.Reader) (BlobHandle, int, error) {
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return BlobHandle{}, 0, err
	}
	return BlobHandle{Slug: slug, SHA: sha}, int(n), nil
}

func (txTestBlobUnit) StageStream(_ context.Context, slug, sha string, r io.Reader, _ int64) (BlobHandle, error) {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return BlobHandle{}, err
	}
	return BlobHandle{Slug: slug, SHA: sha}, nil
}

func (txTestBlobUnit) Commit(ctx context.Context, _ []BlobHandle, metaWrite func(context.Context) error) error {
	return metaWrite(ctx)
}

func (txTestBlobUnit) Read(context.Context, string, string) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("txTestBlobUnit: Read unused")
}

func (txTestBlobUnit) ReadAll(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("txTestBlobUnit: ReadAll unused")
}

func (txTestBlobUnit) UnbindOnDelete(context.Context, string, []string) error { return nil }

func (txTestBlobUnit) IsTransactional() bool { return true }

func claimDeployFixture(t *testing.T) (*DeploySite, *claimSiteRepo) {
	t.Helper()
	sites := &claimSiteRepo{}
	d := NewDeploySite(sites, zeroPasteBytes{}, txTestBlobUnit{})
	return d, sites
}

// claimArchive builds a gzip-tar of files.
func claimArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func assertClaimReleased(t *testing.T, sites *claimSiteRepo) {
	t.Helper()
	if len(sites.claimed) != 1 {
		t.Fatalf("pre-claims: got %d, want 1", len(sites.claimed))
	}
	if len(sites.released) != 1 {
		t.Fatalf("released claims: got %d, want 1 (the claim is durable and nothing else drops it)", len(sites.released))
	}
	if sites.released[0] != sites.claimed[0] {
		t.Fatalf("released %q, want the claimed slug %q", sites.released[0], sites.claimed[0])
	}
}

func TestDeploy_ReleasesPreClaimOnUnreadableArchive(t *testing.T) {
	d, sites := claimDeployFixture(t)

	_, err := d.Deploy(bytes.NewReader([]byte("not a gzip stream at all")), "key:AAAA")
	if !errors.Is(err, domain.ErrUnsupportedKind) {
		t.Fatalf("Deploy err = %v, want ErrUnsupportedKind", err)
	}
	assertClaimReleased(t, sites)
}

func TestDeploy_ReleasesPreClaimOnNoWebContent(t *testing.T) {
	d, sites := claimDeployFixture(t)

	body := claimArchive(t, map[string]string{"notes.txt": "no web content here"})
	_, err := d.Deploy(bytes.NewReader(body), "key:AAAA")
	if !errors.Is(err, domain.ErrNoWebContent) {
		t.Fatalf("Deploy err = %v, want ErrNoWebContent", err)
	}
	assertClaimReleased(t, sites)
}

func TestDeploy_ReleasesPreClaimOnEmptyArchive(t *testing.T) {
	d, sites := claimDeployFixture(t)

	body := claimArchive(t, nil)
	_, err := d.Deploy(bytes.NewReader(body), "key:AAAA")
	if !errors.Is(err, ErrEmptySite) {
		t.Fatalf("Deploy err = %v, want ErrEmptySite", err)
	}
	assertClaimReleased(t, sites)
}

func TestDeploy_ReleasesPreClaimWhenCommitFails(t *testing.T) {
	d, sites := claimDeployFixture(t)
	sites.insertErr = errors.New("backend unavailable")

	body := claimArchive(t, map[string]string{"index.html": "<h1>hi</h1>"})
	if _, err := d.Deploy(bytes.NewReader(body), "key:AAAA"); err == nil {
		t.Fatal("Deploy err = nil, want the insert error")
	}
	assertClaimReleased(t, sites)
}

func TestDeploy_KeepsPreClaimOnSuccess(t *testing.T) {
	d, sites := claimDeployFixture(t)

	body := claimArchive(t, map[string]string{"index.html": "<h1>hi</h1>"})
	res, err := d.Deploy(bytes.NewReader(body), "key:AAAA")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(sites.claimed) != 1 || res.Site.Slug != sites.claimed[0] {
		t.Fatalf("deployed slug %q, want the claimed slug %v", res.Site.Slug, sites.claimed)
	}
	if len(sites.released) != 0 {
		t.Fatalf("released %v on a committed deploy; the marker belongs to the site", sites.released)
	}
}
