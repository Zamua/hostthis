package domain

import "testing"

// octetStreamAssets are the asset extensions whose CORRECT content type IS the
// unknown-bytes default: opaque payloads, and a compression wrapper with no
// registered media type.
var octetStreamAssets = map[string]struct{}{
	".bin": {}, ".dat": {}, ".br": {},
}

// Every extension the SPA fallback admits as a static ASSET resolves to a real
// content type. An asset falling through to application/octet-stream means a
// deployed site serves that file as a download instead of playing or rendering
// it, which is what a second, separately-maintained extension table produced.
func TestAssetExtensionsAllResolveToAContentType(t *testing.T) {
	for ext := range assetExtensions {
		if _, opaque := octetStreamAssets[ext]; opaque {
			continue
		}
		if got := ContentTypeForPath("file" + ext); got == "application/octet-stream" {
			t.Fatalf("asset extension %q resolves to the unknown-extension default %q", ext, got)
		}
	}
}

// The content types media, data and script assets serve as. Pinned by value
// because a wrong type here makes <video>/<audio> sources fail silently.
func TestContentTypeForPath_MediaAndDataAssets(t *testing.T) {
	cases := map[string]string{
		"clip.mp4":    "video/mp4",
		"clip.webm":   "video/webm",
		"clip.mov":    "video/quicktime",
		"clip.m4v":    "video/x-m4v",
		"clip.ogv":    "video/ogg",
		"song.mp3":    "audio/mpeg",
		"song.wav":    "audio/wav",
		"song.ogg":    "audio/ogg",
		"song.flac":   "audio/flac",
		"song.m4a":    "audio/mp4",
		"song.aac":    "audio/aac",
		"rows.csv":    "text/csv; charset=utf-8",
		"old.bmp":     "image/bmp",
		"mod.cjs":     "text/javascript; charset=utf-8",
		"bundle.gz":   "application/gzip",
		"archive.zip": "application/zip",
		"bundle.br":   "application/octet-stream",
		"blob.bin":    "application/octet-stream",
		"blob.dat":    "application/octet-stream",
	}
	for p, want := range cases {
		if got := ContentTypeForPath(p); got != want {
			t.Fatalf("%q: got %q, want %q", p, got, want)
		}
	}
}

// A known content type does NOT make an extension an asset: a missing ".html"
// or ".md" path is a client-side route the SPA fallback serves the index for.
func TestKnownNonAssetExtensionsStayRoutes(t *testing.T) {
	for _, ext := range []string{".html", ".htm", ".md", ".markdown"} {
		if looksLikeAsset("page" + ext) {
			t.Fatalf("%q must not count as an asset: a miss has to fall through to the SPA index", ext)
		}
		if got := ContentTypeForPath("page" + ext); got == "application/octet-stream" {
			t.Fatalf("%q should have a real content type, got %q", ext, got)
		}
	}
	if !looksLikeAsset("/img/LOGO.PNG") {
		t.Fatalf("asset matching must be case-insensitive")
	}
	if got := ContentTypeForPath("/img/LOGO.PNG"); got != "image/png" {
		t.Fatalf("content type must be case-insensitive, got %q", got)
	}
}
