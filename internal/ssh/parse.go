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

// humanDuration formats a remaining-time duration compactly.
// Used in the EXPIRES_IN column of `list` and the footer of
// `versions`. Goes "Nd Hh" for >= 1 day, "NhMm" for >= 1 hour,
// "Nm" for >= 1 minute, "<1m" / "expired" at the edges.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	if d < time.Minute {
		return "<1m"
	}
	totalHours := int(d.Hours())
	days := totalHours / 24
	hours := totalHours - days*24
	minutes := int(d.Minutes()) - totalHours*60
	switch {
	case days >= 1:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours >= 1:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
