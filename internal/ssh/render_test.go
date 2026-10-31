package ssh

import (
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// The list table's NAME and VERS columns, per SPEC.md's "List your pastes":
// an unnamed row shows "-", VERS is latest-served or pinned, and v0 never
// appears for an unpinned paste (PinnedVersion=0 is a sentinel). A site has no
// version timeline.
func TestListTableVersColumn(t *testing.T) {
	cases := []struct {
		name string
		p    domain.Paste
		want string
	}{
		{"unpinned single version", domain.Paste{PinnedVersion: 0, LatestVersion: 1}, "v1"},
		{"unpinned multiple versions", domain.Paste{PinnedVersion: 0, LatestVersion: 4}, "v4"},
		{"pinned to latest", domain.Paste{PinnedVersion: 3, LatestVersion: 3}, "v3 (pinned)"},
		{"pinned to older version", domain.Paste{PinnedVersion: 1, LatestVersion: 5}, "v1 (pinned, latest v5)"},
		{"pinned >= latest (defensive)", domain.Paste{PinnedVersion: 7, LatestVersion: 5}, "v7 (pinned)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.p.Slug, tc.p.Kind = "abcd2345", "html"
			if got := tableRow(t, []domain.Paste{tc.p}, nil); got[1] != "-" || got[4] != tc.want {
				t.Fatalf("row %q, want NAME - and VERS %q", got, tc.want)
			}
		})
	}

	if got := tableRow(t, nil, []domain.Site{{Slug: "site2345", Kind: domain.KindSite}}); got[4] != "-" {
		t.Fatalf("site row %q, want VERS -", got)
	}
}

// tableRow is the list table's only data row, split into its five columns.
func tableRow(t *testing.T, pastes []domain.Paste, sites []domain.Site) []string {
	t.Helper()
	out, _ := listOutput(t, formatTable, pastes, sites)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("table %q, want a header and one row", out)
	}
	f := strings.Fields(lines[1])
	if len(f) < 5 {
		t.Fatalf("row %q has fewer than five columns", lines[1])
	}
	return append(f[:4:4], strings.Join(f[4:], " "))
}
