package ssh

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// output.go is the SSH adapter's PRESENTATION contract for the read
// verbs (`list`, `versions`, `whoami`). The domain + service layers stay
// pure and return domain types; this file is the thin translation layer
// that shapes those types into either the human table (rendered by the
// verb handlers themselves) or a stable, machine-readable JSON document.
//
// Two deliberate DDD choices:
//
//   - The JSON wire shape is a set of dedicated *view* structs, NOT the
//     internal domain types. Marshaling domain.Paste directly would leak
//     internal field names and couple the wire format to refactors; the
//     view structs are the published contract (see docs/SPEC.md).
//   - Machine consumers get real types: integer bytes, RFC 3339
//     timestamps (or null), never the human "2.4k" / "13m" strings.

// outputFormat is the value of the `-o` / `--output` selector.
type outputFormat string

const (
	formatTable outputFormat = "table" // default; the human-aligned tabwriter output
	formatJSON  outputFormat = "json"  // stable JSON document on stdout
)

// parseOutputFormat extracts a kubectl-style `-o <fmt>` / `--output <fmt>`
// (and the `=`-joined `-o=<fmt>` / `--output=<fmt>` forms) from a verb's
// argument list, wherever it appears, and returns the selected format
// plus the remaining positional args with the flag removed. Absent flag
// => formatTable. An unrecognized format value, or a `-o` with no value,
// is a usage error (the caller maps it to ExitUsage).
func parseOutputFormat(argv []string) (outputFormat, []string, error) {
	format := formatTable
	rest := make([]string, 0, len(argv))

	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		var val string
		switch {
		case arg == "-o" || arg == "--output":
			// value is the next token
			if i+1 >= len(argv) {
				return "", nil, fmt.Errorf("missing value for %s (want: table, json)", arg)
			}
			val = argv[i+1]
			i++ // consume the value
		case strings.HasPrefix(arg, "-o="):
			val = arg[len("-o="):] // may be empty -> validated below
		case strings.HasPrefix(arg, "--output="):
			val = arg[len("--output="):]
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

// writeJSON marshals v as indented JSON followed by a newline. Callers
// use this only in json mode, where stdout carries the JSON value alone.
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

// pasteView is one row of `list -o json`.
type pasteView struct {
	Slug             string  `json:"slug"`
	Name             string  `json:"name"` // "" when unset (not the "-" table sentinel)
	SizeBytes        int     `json:"size_bytes"`
	Kind             string  `json:"kind"`
	ExpiresAt        *string `json:"expires_at"`         // RFC3339, null when never-expires
	ExpiresInSeconds *int64  `json:"expires_in_seconds"` // null when never-expires
	ServedVersion    int     `json:"served_version"`
	LatestVersion    int     `json:"latest_version"`
	PinnedVersion    int     `json:"pinned_version"` // 0 when unpinned
}

// newPasteView maps a domain.Paste to its list view relative to now.
func newPasteView(p domain.Paste, now time.Time) pasteView {
	at, in := expiryFields(p.ExpiresAt, now)
	return pasteView{
		Slug:             string(p.Slug),
		Name:             p.Name,
		SizeBytes:        p.Size,
		Kind:             string(p.Kind),
		ExpiresAt:        at,
		ExpiresInSeconds: in,
		ServedVersion:    servedVersion(p.PinnedVersion, p.LatestVersion),
		LatestVersion:    p.LatestVersion,
		PinnedVersion:    p.PinnedVersion,
	}
}

// newPasteViews maps a slice, guaranteeing a non-nil slice so the JSON is
// `[]` (not `null`) when the owner has no active pastes.
func newPasteViews(pastes []domain.Paste, now time.Time) []pasteView {
	views := make([]pasteView, 0, len(pastes))
	for _, p := range pastes {
		views = append(views, newPasteView(p, now))
	}
	return views
}

// versionsView is the `versions <slug> -o json` document. It folds the
// stderr footer (pin state + paste expiry) into the object around the
// version array.
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
// non-deleted ver_num when unpinned) computed by the caller, which
// already walks the list for the table render.
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
// prefix is stripped to match the human render (ssh-keygen -lf style);
// quota_bytes is null when there's no cap; session is null when the
// keygate isn't wired (no subnet).
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

// expiryFields renders (expires_at, expires_in_seconds) for a paste
// expiry: both nil when the paste never expires; otherwise an RFC3339
// timestamp and the whole seconds until expiry (clamped at 0, never
// negative, so an already-expired-but-not-yet-swept paste reads 0).
func expiryFields(expiresAt, now time.Time) (*string, *int64) {
	if expiresAt.Equal(domain.NeverExpires) {
		return nil, nil
	}
	at := expiresAt.UTC().Format(time.RFC3339)
	secs := max(int64(expiresAt.Sub(now).Seconds()), 0)
	return &at, &secs
}

// servedVersion is the version the URL serves: the pin when set, else the
// latest. Mirrors renderVersCol's logic for the json shape.
func servedVersion(pinned, latest int) int {
	if pinned != 0 {
		return pinned
	}
	return latest
}
