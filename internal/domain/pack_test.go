package domain

import "testing"

// Small files pack, large or unknown-size files stand alone.
func TestPackable(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want bool
	}{
		{0, true},
		{PackMemberMaxBytes, true},
		{PackMemberMaxBytes + 1, false},
		{-1, false},
	} {
		if got := Packable(tc.size); got != tc.want {
			t.Errorf("Packable(%d) = %v, want %v", tc.size, got, tc.want)
		}
	}
}

func TestPackFull(t *testing.T) {
	if PackFull(PackTargetBytes - 1) {
		t.Error("PackFull just under the target = true")
	}
	if !PackFull(PackTargetBytes) {
		t.Error("PackFull at the target = false")
	}
}

// A packed entry's validator tells apart files sharing one pack; a whole-object
// entry keeps its key, so existing ETags do not change.
func TestManifestEntryValidator(t *testing.T) {
	whole := ManifestEntry{Key: "uploads/u/0"}
	if got := whole.Validator(); got != "uploads/u/0" {
		t.Errorf("whole validator = %q", got)
	}
	a := ManifestEntry{Key: "uploads/u/1", Offset: 0, Length: 10}
	b := ManifestEntry{Key: "uploads/u/1", Offset: 10, Length: 7}
	if a.Validator() == b.Validator() {
		t.Errorf("packed entries share validator %q", a.Validator())
	}
	if got := b.Ref(); got != (BlobRef{Key: "uploads/u/1", Offset: 10, Length: 7}) {
		t.Errorf("Ref = %+v", got)
	}
}
