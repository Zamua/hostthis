package ssh

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// The SSH adapter's PRESENTATION contract for the read verbs (`list`,
// `versions`, `whoami`): domain types in, human table or machine-readable JSON
// out.
//
//   - The JSON wire shape is a set of dedicated *view* structs, NOT the domain
//     types. Marshaling domain.Paste directly would leak internal field names
//     and couple the wire format to refactors; the view structs are the
//     published contract (docs/SPEC.md).
//   - Machine consumers get real types: integer bytes, RFC 3339 timestamps (or
//     null), never the human "2.4k" / "13m" strings.

// outputFormat is the value of the `-o` / `--output` selector.
type outputFormat string

const (
	formatTable outputFormat = "table" // default; the human-aligned tabwriter output
	formatJSON  outputFormat = "json"  // stable JSON document on stdout
)

// parseOutputFormat extracts a kubectl-style output selector from anywhere in
// a verb's argument list and returns the format plus the remaining positional
// args. Every pflag-style spelling is accepted: `-o <fmt>`, `--output <fmt>`,
// `-o=<fmt>`, `--output=<fmt>`, and the glued short form `-o<fmt>` (e.g.
// `-ojson`). Absent flag => formatTable. An unrecognized value, or a bare `-o`,
// is a usage error the caller maps to ExitUsage.
func parseOutputFormat(argv []string) (outputFormat, []string, error) {
	format := formatTable
	rest := make([]string, 0, len(argv))

	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		var val string
		switch {
		case arg == "-o" || arg == "--output":
			if i+1 >= len(argv) {
				return "", nil, fmt.Errorf("missing value for %s (want: table, json)", arg)
			}
			val = argv[i+1]
			i++ // consume the value
		case strings.HasPrefix(arg, "-o="):
			val = arg[len("-o="):] // may be empty -> validated below
		case strings.HasPrefix(arg, "--output="):
			val = arg[len("--output="):]
		case strings.HasPrefix(arg, "-o"):
			// Glued short form ("-ojson"). Must stay after the "-o=" case so
			// "-o=json" is not caught here.
			val = arg[len("-o"):]
		default:
			rest = append(rest, arg)
			continue
		}

		f, err := parseFormatValue(val)
		if err != nil {
			return "", nil, err
		}
		format = f
	}

	return format, rest, nil
}

func parseFormatValue(val string) (outputFormat, error) {
	switch outputFormat(val) {
	case formatTable:
		return formatTable, nil
	case formatJSON:
		return formatJSON, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want: table, json)", val)
	}
}

// writeJSON marshals v as indented JSON followed by a newline. Used only in
// json mode, where stdout carries the JSON value alone.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

// ---------------------------------------------------------------------------
// View structs (the published JSON contract) + mappers from domain types.
// ---------------------------------------------------------------------------

// listItemView is one row of `list -o json`: a text paste OR a static site,
// discriminated by Kind ("site" for sites). The version fields are *int so an
// unversioned site serializes them as null. Expiry fields are null for a
// never-expiring item.
type listItemView struct {
	Slug             string  `json:"slug"`
	Name             string  `json:"name"` // "" when unset (not the "-" table sentinel)
	SizeBytes        int     `json:"size_bytes"`
	Kind             string  `json:"kind"`               // html/markdown/diff, or "site"
	ExpiresAt        *string `json:"expires_at"`         // RFC3339, null when never-expires
	ExpiresInSeconds *int64  `json:"expires_in_seconds"` // null when never-expires
	ServedVersion    *int    `json:"served_version"`     // null for sites
	LatestVersion    *int    `json:"latest_version"`     // null for sites
	PinnedVersion    *int    `json:"pinned_version"`     // null for sites; 0 when unpinned

	expiresAt time.Time // raw expiry for merge-sort; not serialized
}

func newPasteListItem(p domain.Paste, now time.Time) listItemView {
	at, in := expiryFields(p.ExpiresAt, now)
	served := servedVersion(p.PinnedVersion, p.LatestVersion)
	latest := p.LatestVersion
	pinned := p.PinnedVersion
	return listItemView{
		Slug:             string(p.Slug),
		Name:             p.Name,
		SizeBytes:        p.Size,
		Kind:             string(p.Kind),
		ExpiresAt:        at,
		ExpiresInSeconds: in,
		ServedVersion:    &served,
		LatestVersion:    &latest,
		PinnedVersion:    &pinned,
		expiresAt:        p.ExpiresAt,
	}
}

// newSiteListItem maps a domain.Site to a list item. Sites have no label and
// no versions; SizeBytes is what the quota charged, so a list sums to the
// figure whoami reports.
func newSiteListItem(s domain.Site, now time.Time) listItemView {
	at, in := expiryFields(s.ExpiresAt, now)
	return listItemView{
		Slug:             string(s.Slug),
		Name:             "",
		SizeBytes:        s.StoredBytes,
		Kind:             "site",
		ExpiresAt:        at,
		ExpiresInSeconds: in,
		// version fields nil: sites are not versioned
		expiresAt: s.ExpiresAt,
	}
}

// newListView merges pastes + sites into one slice sorted by expiry ascending,
// so never-expiring items sort last. Guaranteed non-nil so json renders `[]`
// rather than `null` when the owner has no active content.
func newListView(pastes []domain.Paste, sites []domain.Site, now time.Time) []listItemView {
	views := make([]listItemView, 0, len(pastes)+len(sites))
	for _, p := range pastes {
		views = append(views, newPasteListItem(p, now))
	}
	for _, s := range sites {
		views = append(views, newSiteListItem(s, now))
	}
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].expiresAt.Before(views[j].expiresAt)
	})
	return views
}

// versCol renders the table VERS column: "-" for an unversioned site, else the
// paste's version state (mirrors renderVersCol).
func versCol(v listItemView) string {
	if v.ServedVersion == nil {
		return "-"
	}
	switch *v.PinnedVersion {
	case 0:
		return fmt.Sprintf("v%d", *v.LatestVersion)
	case *v.LatestVersion:
		return fmt.Sprintf("v%d (pinned)", *v.LatestVersion)
	default:
		return fmt.Sprintf("v%d (pinned, latest v%d)", *v.PinnedVersion, *v.LatestVersion)
	}
}

// versionsView is the `versions <slug> -o json` document. It folds the stderr
// footer (pin state + paste expiry) into the object around the version array.
type versionsView struct {
	Slug          string        `json:"slug"`
	PinnedVersion int           `json:"pinned_version"` // 0 when unpinned
	ExpiresAt     *string       `json:"expires_at"`     // RFC3339, null when never-expires
	Versions      []versionView `json:"versions"`
}

// versionView is one row of the version timeline.
type versionView struct {
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"` // RFC3339
	SizeBytes *int   `json:"size_bytes"` // null for a deleted (tombstoned) version
	Deleted   bool   `json:"deleted"`
	Current   bool   `json:"current"` // the version the URL currently serves
}

// newVersionsView maps the version timeline + owning paste to the json
// document. servedVer is the currently-served ver_num (the pin, or MAX
// non-deleted ver_num when unpinned), passed in because the caller already
// walks the list for the table render.
func newVersionsView(slug string, p domain.Paste, vers []domain.Version, servedVer int, now time.Time) versionsView {
	at, _ := expiryFields(p.ExpiresAt, now)
	views := make([]versionView, 0, len(vers))
	for _, v := range vers {
		vv := versionView{
			Version:   v.VerNum,
			CreatedAt: v.CreatedAt.UTC().Format(time.RFC3339),
			Deleted:   v.Deleted,
			Current:   v.VerNum == servedVer,
		}
		if !v.Deleted {
			size := v.Size
			vv.SizeBytes = &size
		}
		views = append(views, vv)
	}
	return versionsView{
		Slug:          slug,
		PinnedVersion: p.PinnedVersion,
		ExpiresAt:     at,
		Versions:      views,
	}
}

// whoamiView is the `whoami -o json` document.
type whoamiView struct {
	Key          string       `json:"key"`
	FirstSeen    *string      `json:"first_seen"` // RFC3339, null when unknown/zero
	ActivePastes int          `json:"active_pastes"`
	UsedBytes    int          `json:"used_bytes"`
	QuotaBytes   *int         `json:"quota_bytes"` // null when the owner has no quota cap
	Session      *sessionView `json:"session"`     // null when the keygate isn't wired / no subnet
}

type sessionView struct {
	Subnet           string `json:"subnet"`
	IdentitySubnets  int    `json:"identity_subnets"`
	SubnetFreshCount int    `json:"subnet_fresh_count"`
	SubnetCap        int    `json:"subnet_cap"`
}

// newWhoamiView maps the service WhoamiInfo to its json document. The key
// prefix is stripped to match the human render (ssh-keygen -lf style).
func newWhoamiView(info service.WhoamiInfo) whoamiView {
	v := whoamiView{
		Key:          strings.TrimPrefix(info.Identity, domain.IdentityKeyPrefix),
		ActivePastes: info.Active,
		UsedBytes:    info.UsedBytes,
	}
	if !info.FirstSeen.IsZero() {
		fs := info.FirstSeen.UTC().Format(time.RFC3339)
		v.FirstSeen = &fs
	}
	if info.QuotaBytes > 0 {
		q := info.QuotaBytes
		v.QuotaBytes = &q
	}
	if info.Session.Subnet != "" {
		v.Session = &sessionView{
			Subnet:           info.Session.Subnet,
			IdentitySubnets:  info.Session.IdentitySubnets,
			SubnetFreshCount: info.Session.SubnetFreshCount,
			SubnetCap:        info.Session.SubnetCap,
		}
	}
	return v
}

// expiryFields renders (expires_at, expires_in_seconds): both nil when the
// paste never expires, otherwise an RFC3339 timestamp and the whole seconds
// until expiry, clamped at 0 so an expired-but-unswept paste reads 0 rather
// than negative.
func expiryFields(expiresAt, now time.Time) (*string, *int64) {
	if expiresAt.Equal(domain.NeverExpires) {
		return nil, nil
	}
	at := expiresAt.UTC().Format(time.RFC3339)
	secs := max(int64(expiresAt.Sub(now).Seconds()), 0)
	return &at, &secs
}

// servedVersion is the version the URL serves: the pin when set, else the
// latest.
func servedVersion(pinned, latest int) int {
	if pinned != 0 {
		return pinned
	}
	return latest
}
