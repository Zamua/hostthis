package domain

import (
	"strings"
	"testing"
)

// A pod upload id stays a valid upload id and names its link.
func TestPodUploadIDNamesItsLink(t *testing.T) {
	link := NewPodLinkID()
	if !ValidPodLinkID(link) {
		t.Fatalf("NewPodLinkID() = %q, not valid", link)
	}
	id := NewPodUploadID(link)
	if !ValidUploadID(id) {
		t.Fatalf("NewPodUploadID = %q, not a valid upload id", id)
	}
	if got, ok := PodLinkOfUploadID(id); !ok || got != link {
		t.Fatalf("PodLinkOfUploadID(%q) = %q, %v; want %q", id, got, ok, link)
	}
	if _, ok := PodLinkOfUploadID(NewUploadID()); ok {
		t.Fatal("a bucket upload id names a link")
	}
}

func TestValidPodLinkID(t *testing.T) {
	for _, bad := range []string{"", "abc", strings.Repeat("g", 16), strings.Repeat("A", 16), strings.Repeat("a", 17)} {
		if ValidPodLinkID(bad) {
			t.Errorf("ValidPodLinkID(%q) = true", bad)
		}
	}
}

// The link URL secret is hashed the way the Worker checks it.
func TestPodLinkSecretHash(t *testing.T) {
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := PodLinkSecretHash("hello"); got != want {
		t.Fatalf("PodLinkSecretHash = %s, want %s", got, want)
	}
}

func TestParseWebID(t *testing.T) {
	for _, good := range []string{"https://alice.example/profile/card#me", "http://localhost:3000/alice/profile/card#me"} {
		if _, err := ParseWebID(good); err != nil {
			t.Errorf("ParseWebID(%q) = %v", good, err)
		}
	}
	for _, bad := range []string{"", "alice", "ftp://x/y", "http://alice.example/card#me", "https://", "https://a.example/\n"} {
		if _, err := ParseWebID(bad); err == nil {
			t.Errorf("ParseWebID(%q) succeeded", bad)
		}
	}
}
