package ssh

import (
	"errors"
	"fmt"

	gossh "github.com/charmbracelet/ssh"

	"github.com/Zamua/hostthis/internal/domain"
)

// podView is `pod status -o json`; null when no pod is linked.
type podView struct {
	Pod    string `json:"pod"`
	WebID  string `json:"webid"`
	Linked string `json:"linked"`
}

// verbPod links, shows and unlinks the owner's Solid pod (docs/SPEC.md "Pod").
func (s *Server) verbPod(sess gossh.Session, owner domain.Owner, argv []string) {
	if s.Pods == nil || s.PodLinkURL == nil {
		emitServiceErr(sess, domain.ErrPodsUnavailable)
		return
	}
	if len(argv) == 0 {
		emitUsageErr(sess, errors.New("usage: pod link <webid> | pod status | pod unlink"))
		return
	}
	switch argv[0] {
	case "link":
		if len(argv) != 2 {
			emitUsageErr(sess, errors.New("usage: pod link <webid>"))
			return
		}
		if _, err := domain.ParseWebID(argv[1]); err != nil {
			emitUsageErr(sess, err)
			return
		}
		start, err := s.Pods.Link(owner, argv[1])
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
		linkURL := s.PodLinkURL(start.ID, start.Secret)
		_, _ = fmt.Fprintf(sess, "open within %d minutes to link %s:\n%s\n",
			int(domain.PodLinkTTL.Minutes()), owner.Fingerprint(), linkURL)
		// Usually opened on a phone, like an upload's URL.
		writeQR(sess.Stderr(), linkURL)
	case "status":
		format, rest, err := parseOutputFormat(argv[1:])
		if err != nil || len(rest) > 0 {
			emitUsageErr(sess, errors.New("usage: pod status [-o json]"))
			return
		}
		link, err := s.Pods.Status(owner)
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
		if format == formatJSON {
			var view *podView
			if link != nil {
				view = &podView{Pod: link.Storage + "hostthis/", WebID: link.WebID, Linked: link.Linked.Format("2006-01-02")}
			}
			if err := writeJSON(sess, view); err != nil {
				emitServiceErr(sess, err)
				return
			}
			break
		}
		if link == nil {
			_, _ = fmt.Fprintln(sess, "no pod linked.")
			break
		}
		_, _ = fmt.Fprintf(sess, "pod:     %shostthis/\nwebid:   %s\nlinked:  %s\n",
			link.Storage, link.WebID, link.Linked.Format("2006-01-02"))
	case "unlink":
		if len(argv) != 1 {
			emitUsageErr(sess, errors.New("usage: pod unlink"))
			return
		}
		had, err := s.Pods.Unlink(owner)
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
		if !had {
			_, _ = fmt.Fprintln(sess, "no pod linked.")
			break
		}
		_, _ = fmt.Fprintln(sess, "unlinked. new uploads are stored by hostthis; versions in your pod stay there.")
	default:
		emitUsageErr(sess, fmt.Errorf("unknown pod command %q (want link, status or unlink)", argv[0]))
		return
	}
	_ = sess.Exit(ExitOK)
}
