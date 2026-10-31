package domain

import (
	"errors"
	"testing"
)

func TestOwnerFromKeyFingerprint(t *testing.T) {
	o, err := OwnerFromKeyFingerprint("SHA256:ab12")
	if err != nil {
		t.Fatal(err)
	}
	if o.String() != "key:SHA256:ab12" || o.Identity() != Identity("key:SHA256:ab12") || o.Fingerprint() != "SHA256:ab12" {
		t.Fatalf("owner = %q / %q / %q", o.String(), o.Identity(), o.Fingerprint())
	}
	if _, err := OwnerFromKeyFingerprint(""); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("empty fingerprint: err = %v, want ErrNoOwner", err)
	}
}

func TestParseOwnerRoundTripsAndRefusesUnkeyed(t *testing.T) {
	o, err := ParseOwner("key:SHA256:ab12")
	if err != nil {
		t.Fatal(err)
	}
	if o.String() != "key:SHA256:ab12" {
		t.Fatalf("ParseOwner = %q", o.String())
	}
	for _, s := range []string{"", "key:", "SHA256:ab12", "anon:x", " key:SHA256:ab12"} {
		if _, err := ParseOwner(s); !errors.Is(err, ErrNoOwner) {
			t.Errorf("ParseOwner(%q) err = %v, want ErrNoOwner", s, err)
		}
	}
}
