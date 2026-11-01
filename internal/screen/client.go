// Package screen is the HTTP adapter for the pre-publish screening service
// (docs/SPEC.md "Pre-publish screening"). What the service checks lives with
// the deployment, not here.
package screen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// Client posts uploads to <Base>/screen.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func (c *Client) Screen(ctx context.Context, req domain.ScreenRequest) (domain.ScreenVerdict, error) {
	body, size, err := sized(req.Body)
	if err != nil {
		return domain.ScreenVerdict{}, err
	}
	if closer, ok := body.(io.Closer); ok && body != req.Body {
		defer closer.Close() //nolint:errcheck
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.Base, "/")+"/screen", body)
	if err != nil {
		return domain.ScreenVerdict{}, err
	}
	// The service needs a length up front to refuse an oversize body.
	hreq.ContentLength = size
	hreq.Header.Set("X-Hostthis-Shape", string(req.Shape))
	if req.Name != "" {
		hreq.Header.Set("X-Hostthis-Name", req.Name)
	}
	if req.Truncated {
		hreq.Header.Set("X-Hostthis-Truncated", "1")
	}
	if c.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(hreq)
	if err != nil {
		return domain.ScreenVerdict{}, fmt.Errorf("screen: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck
	if res.StatusCode != http.StatusOK {
		return domain.ScreenVerdict{}, fmt.Errorf("screen: status %d", res.StatusCode)
	}
	var v struct {
		Allow *bool  `json:"allow"`
		Rule  string `json:"rule"`
		File  string `json:"file"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&v); err != nil || v.Allow == nil {
		return domain.ScreenVerdict{}, fmt.Errorf("screen: unreadable answer")
	}
	return domain.ScreenVerdict{Allow: *v.Allow, Rule: v.Rule, File: v.File}, nil
}

// sized gives the body a known length: a file by its size, anything else
// spilled to a temporary file.
func sized(r io.Reader) (io.Reader, int64, error) {
	if f, ok := r.(*os.File); ok {
		info, err := f.Stat()
		if err != nil {
			return nil, 0, err
		}
		off, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}
		return io.NopCloser(f), info.Size() - off, nil
	}
	spill, err := os.CreateTemp("", "hostthis-screen-body-*")
	if err != nil {
		return nil, 0, err
	}
	_ = os.Remove(spill.Name())
	n, err := io.Copy(spill, r)
	if err == nil {
		_, err = spill.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = spill.Close()
		return nil, 0, err
	}
	return spill, n, nil
}
