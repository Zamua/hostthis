package service

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// used_bytes is the figure the quota charges, across pastes and sites, and
// first_seen and active follow the owner's uploads.
func TestWhoami_ReportsTheQuotaFigure(t *testing.T) {
	blobs, repo := realBlobs(t), newRepo(t)
	upload, manage := NewUpload(repo, blobs), NewManage(repo, blobs)
	deploy := NewDeploySite(storage.NewSites(repo), repo, blobs)
	fixedNowFn := func() time.Time { return fixedNow }
	upload.Now, manage.Now, deploy.Now = fixedNowFn, fixedNowFn, fixedNowFn
	owner := ownerOf("key:whoami")

	empty, err := manage.Whoami(owner, "")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if empty.Active != 0 || !empty.FirstSeen.IsZero() || empty.UsedBytes != 0 || empty.QuotaBytes != domain.UserQuotaBytes {
		t.Fatalf("new owner: %+v", empty)
	}

	if _, err := upload.Create(strings.NewReader("# a paste\n\nbody\n"), owner, "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	upload.WaitFinalize()
	pasted, err := manage.Whoami(owner, "")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if pasted.Active != 1 || !pasted.FirstSeen.Equal(fixedNow) || pasted.UsedBytes <= 0 {
		t.Fatalf("after a paste: %+v", pasted)
	}

	if _, err := deploy.Deploy(bytes.NewReader(gzipTar(t, map[string]string{"index.html": "<h1>hi</h1>"})), owner); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	both, err := manage.Whoami(owner, "")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if charged := ownerCharge(t, owner.String()); both.Active != 2 || both.UsedBytes <= pasted.UsedBytes || int64(both.UsedBytes) != charged {
		t.Fatalf("with a site: %+v, want 2 active and the %d bytes the quota charges", both, charged)
	}
}
