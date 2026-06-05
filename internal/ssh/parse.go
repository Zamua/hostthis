package ssh

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// uploadArgs captures the parsed argv for upload/update. Only one of
// Slug (update) or Name (create-with-name) is the headline; both can
// be set if `cat foo | ssh hostthis.dev <slug> --name "new"` resets
// the label as part of an update.
type uploadArgs struct {
	Slug string
	Name string
	Type string // "html", "md", etc. — passed straight to DetectKind
}

// parseUploadFlags consumes the upload argv:
//
//	[<slug>] [--name "label"] [--type html|md]
//
// Flags can come before or after the slug; the parser stays minimal.
func parseUploadFlags(argv []string) (uploadArgs, error) {
	var out uploadArgs
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--name":
			if i+1 >= len(argv) {
				return out, fmt.Errorf("--name needs a value")
			}
			out.Name = argv[i+1]
			i++
		case strings.HasPrefix(a, "--name="):
			out.Name = strings.TrimPrefix(a, "--name=")
		case a == "--type":
			if i+1 >= len(argv) {
				return out, fmt.Errorf("--type needs a value")
			}
			out.Type = argv[i+1]
			i++
		case strings.HasPrefix(a, "--type="):
			out.Type = strings.TrimPrefix(a, "--type=")
		default:
			// First non-flag positional is the slug.
			if out.Slug == "" {
				if _, err := domain.ParseSlug(a); err == nil {
					out.Slug = a
					continue
				}
			}
			return out, fmt.Errorf("unexpected argument %q", a)
		}
	}
	return out, nil
}

// parseLifetimeFlag handles `--expires <dur>` for the `link` verb.
// Accepts Go duration syntax (`24h`, `7d` shorthand, `never` → 0).
func parseLifetimeFlag(argv []string) (time.Duration, error) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		var raw string
		switch {
		case a == "--expires":
			if i+1 >= len(argv) {
				return 0, fmt.Errorf("--expires needs a value")
			}
			raw = argv[i+1]
			i++
		case strings.HasPrefix(a, "--expires="):
			raw = strings.TrimPrefix(a, "--expires=")
		default:
			return 0, fmt.Errorf("unexpected argument %q", a)
		}
		if raw == "never" {
			return 0, nil // service caps to MaxShareLinkLifetime
		}
		// Shorthand: "Nd" → "N*24h"
		if strings.HasSuffix(raw, "d") {
			n, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid --expires %q", raw)
			}
			return time.Duration(n) * 24 * time.Hour, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("invalid --expires %q", raw)
		}
		return d, nil
	}
	return 0, nil // default lifetime in service
}

// parseInt is a thin alias to keep the call sites short.
func parseInt(s string) (int, error) { return strconv.Atoi(s) }

// humanBytes formats a byte count compactly: 540B, 1.2k, 3.8M.
func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}

// humanDuration formats a remaining-time duration as "Xh Ym",
// "Ym Xs", or "<1m" / "expired" for edge cases. Designed for the
// EXPIRES_IN column in `list` and the footer of `versions`.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	if d < time.Minute {
		return "<1m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	switch {
	case h >= 1:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
