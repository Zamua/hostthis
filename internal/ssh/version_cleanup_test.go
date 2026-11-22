package ssh_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// sixVersions uploads v1..v6 of one paste over ssh and returns its slug.
func sixVersions(t *testing.T, s *stack) string {
	t.Helper()
	stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	slug := extractSlug(stdout)
	for i := 2; i <= 6; i++ {
		if _, stderr, exit := s.run(slug, []byte(fmt.Sprintf("<!doctype html><p>v%d</p>", i))); exit != 0 {
			t.Fatalf("update v%d: %d %q", i, exit, stderr)
		}
	}
	return slug
}

// liveVersions lists the version numbers `versions` shows without a deleted marker.
func liveVersions(t *testing.T, s *stack, slug string) []string {
	t.Helper()
	out, _, _ := s.run("versions "+slug, nil)
	var live []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 && !strings.Contains(line, "deleted") {
			live = append(live, f[0])
		}
	}
	return live
}

func TestDeleteRange_SkipsServed(t *testing.T) {
	s := startStack(t)
	slug := sixVersions(t, s)
	stdout, stderr, exit := s.run("delete "+slug+" 2-6", nil)
	if exit != 0 || !strings.Contains(stderr, "deleted 4 version(s). freed ") {
		t.Fatalf("range delete: exit %d, stderr %q", exit, stderr)
	}
	if got := strings.Fields(stdout); strings.Join(got, " ") != "v5 v4 v3 v2" {
		t.Fatalf("range delete stdout = %q", stdout)
	}
	if got := strings.Join(liveVersions(t, s, slug), " "); got != "v6 v1" {
		t.Fatalf("live after range delete = %q, want v6 v1", got)
	}
}

func TestDeleteRange_BadRangeIsUsage(t *testing.T) {
	s := startStack(t)
	slug := sixVersions(t, s)
	if _, stderr, exit := s.run("delete "+slug+" 5-2", nil); exit != 2 || !strings.Contains(stderr, "invalid version range") {
		t.Fatalf("bad range: exit %d, stderr %q", exit, stderr)
	}
}

func TestPrune_DryRunThenPrune(t *testing.T) {
	s := startStack(t)
	slug := sixVersions(t, s)
	stdout, stderr, exit := s.run("prune "+slug+" --dry-run", nil)
	if exit != 0 || !strings.Contains(stderr, "would delete 2 version(s)") {
		t.Fatalf("dry run: exit %d, stderr %q", exit, stderr)
	}
	if !strings.HasPrefix(stdout, "v2\t") || !strings.Contains(stdout, "\nv1\t") {
		t.Fatalf("dry run stdout = %q", stdout)
	}
	if got := len(liveVersions(t, s, slug)); got != 6 {
		t.Fatalf("dry run deleted versions: %d live", got)
	}
	if _, stderr, exit := s.run("prune "+slug+" --keep 1", nil); exit != 0 || !strings.Contains(stderr, "deleted 4 version(s)") {
		t.Fatalf("prune: exit %d, stderr %q", exit, stderr)
	}
	if got := strings.Join(liveVersions(t, s, slug), " "); got != "v6 v5" {
		t.Fatalf("live after prune = %q, want v6 v5", got)
	}
}

func TestPrune_OtherOwnerIsNotFound(t *testing.T) {
	s := startStack(t)
	slug := sixVersions(t, s)
	if _, _, exit := s.runAnon("prune "+slug, nil); exit == 0 {
		t.Fatal("an anonymous key pruned someone else's paste")
	}
}

// usedBytes reads whoami's used_bytes for the stack's keyed client.
func usedBytes(t *testing.T, s *stack) int {
	t.Helper()
	out, _, _ := s.run("whoami -o json", nil)
	var used int
	if _, after, ok := strings.Cut(out, `"used_bytes":`); ok {
		_, _ = fmt.Sscanf(after, "%d", &used)
	}
	if used == 0 {
		t.Fatalf("no used_bytes in whoami: %q", out)
	}
	return used
}

func TestUpload_WarnsNearFullQuota(t *testing.T) {
	s := startStack(t)
	stdout, stderr, _ := s.run("", []byte("<!doctype html><p>first</p>"))
	slug := extractSlug(stdout)
	_, stderr2, _ := s.run(slug, []byte("<!doctype html><p>second version</p>"))
	if strings.Contains(stderr+stderr2, "quota") {
		t.Fatalf("warned far below the quota: %q %q", stderr, stderr2)
	}
	u1 := usedBytes(t, s)
	old := domain.UserQuotaBytes
	t.Cleanup(func() { domain.UserQuotaBytes = old })
	// A third version the size of the second lands at about 90% of this quota.
	domain.UserQuotaBytes = u1 * 3 / 2 * 110 / 100
	_, stderr, exit := s.run(slug, []byte("<!doctype html><p>third version!</p>"))
	if exit != 0 {
		t.Fatalf("update refused: %d %q", exit, stderr)
	}
	if !strings.Contains(stderr, "note: quota ") || !strings.Contains(stderr, "prune "+slug+" --dry-run") {
		t.Fatalf("no quota warning naming the slug: %q", stderr)
	}
}
