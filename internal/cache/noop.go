// Package cache holds CachePurger implementations. The interface is
// declared in internal/service; concrete adapters live here so the
// service layer doesn't import any specific CDN's SDK.
package cache

// Noop is the default purger: every CDN-affecting call is a silent
// no-op. Used when no CDN is configured in front of hostthis, or when
// the operator wants to rely solely on Cache-Control TTL expiry.
type Noop struct{}

func (Noop) PurgeURLs(urls []string) error { return nil }
