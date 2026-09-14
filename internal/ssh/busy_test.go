package ssh_test

import (
	"context"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

// busyAppendRepo refuses every append because another change is settling.
type busyAppendRepo struct{ *storage.MemRepo }

func (busyAppendRepo) AppendVersionWithQuotaCheck(context.Context, domain.Slug, string, domain.ContentKind,
	string, domain.Manifest, int, int64, time.Time,
) (domain.AppendResult, error) {
	return domain.AppendResult{}, domain.ErrBusy
}

// A busy paste tells the user to retry, without the storage sentinel's prefix.
func TestUpdate_BusyPasteSaysRetry(t *testing.T) {
	s := startStack(t, withManageRepo(func(r *storage.MemRepo) service.PasteAdmin { return busyAppendRepo{r} }))
	stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	slug := extractSlug(stdout)

	_, stderr, exit := s.run(slug, []byte("<!doctype html><p>v2</p>"))
	if exit != hostssh.ExitErr {
		t.Fatalf("exit = %d, want %d (stderr %q)", exit, hostssh.ExitErr, stderr)
	}
	if want := "hostthis: another change to this paste is still settling; retry shortly\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}
