package ssh

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// uploadArgs captures the parsed argv for upload/update. Slug and Name can both
// be set: `... <slug> --name "new"` relabels as part of an update.
type uploadArgs struct {
	Slug string
	Name string
	Type string // "html", "md", etc.; passed straight to DetectKind
}

// parseUploadFlags consumes the upload argv:
//
//	[<slug>] [--name "label"] [--type html|markdown|diff]
//
// Flags may come before or after the slug.
func parseUploadFlags(argv []string) (uploadArgs, error) {
	var out uploadArgs
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--name":
			// ssh flattens the command to a space-joined string, so a
			// multi-word label arrives as several tokens: join up to the next
			// flag. A positional after --name is therefore swallowed by the
			// label, so the slug must precede it.
			var parts []string
			for j := i + 1; j < len(argv) && !strings.HasPrefix(argv[j], "--"); j++ {
				parts = append(parts, argv[j])
			}
			if len(parts) == 0 {
				return out, fmt.Errorf("--name needs a value")
			}
			out.Name = strings.Join(parts, " ")
			i += len(parts)
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
			// The first non-flag positional is the slug.
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

// humanExpiresIn formats the EXPIRES_IN cell for a paste/site, rendering the
// no-expiry sentinel as "never" rather than a nonsensical far-future duration.
func humanExpiresIn(expiresAt, now time.Time) string {
	if expiresAt.Equal(domain.NeverExpires) {
		return "never"
	}
	return humanDuration(expiresAt.Sub(now))
}

// humanDuration renders a remaining time as "2d3h", "3h4m", "5m", "<1m", or
// "expired".
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
