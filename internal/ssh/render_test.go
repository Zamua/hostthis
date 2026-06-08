package ssh

import (
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// TestRenderVersCol pins the three VERS-column states from SPEC.md's
// "List your pastes" section. Was a bare `v<PinnedVersion>` before
// 2026-06-07; that rendered "v0" for unpinned pastes (the column's
// default value), which was useless.
func TestRenderVersCol(t *testing.T) {
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
			got := renderVersCol(tc.p)
			if got != tc.want {
				t.Fatalf("renderVersCol: got %q want %q", got, tc.want)
			}
		})
	}
}
