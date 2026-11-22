package domain

import (
	"reflect"
	"testing"
)

// history builds a newest-first timeline from 1..n, tombstoning dead.
func history(n int, dead ...int) []Version {
	gone := map[int]bool{}
	for _, d := range dead {
		gone[d] = true
	}
	var out []Version
	for v := n; v >= 1; v-- {
		out = append(out, Version{VerNum: v, Size: 10, Deleted: gone[v]})
	}
	return out
}

func nums(vs []Version) []int {
	out := []int{}
	for _, v := range vs {
		out = append(out, v.VerNum)
	}
	return out
}

func TestServedVersion(t *testing.T) {
	if got := ServedVersion(history(5, 5), 0); got != 4 {
		t.Fatalf("unpinned served = %d, want 4", got)
	}
	if got := ServedVersion(history(5), 2); got != 2 {
		t.Fatalf("pinned served = %d, want 2", got)
	}
	if got := ServedVersion(history(2, 1, 2), 0); got != 0 {
		t.Fatalf("all deleted served = %d, want 0", got)
	}
}

func TestParseVersionRange(t *testing.T) {
	lo, hi, ok, err := ParseVersionRange("v3-120")
	if !ok || err != nil || lo != 3 || hi != 120 {
		t.Fatalf("got %d %d %v %v", lo, hi, ok, err)
	}
	if _, _, ok, _ := ParseVersionRange("7"); ok {
		t.Fatal("a single version is not a range")
	}
	for _, bad := range []string{"0-3", "5-2", "a-3", "3-", "-3"} {
		if _, _, ok, err := ParseVersionRange(bad); !ok || err == nil {
			t.Fatalf("%q: want a range error, got ok=%v err=%v", bad, ok, err)
		}
	}
}

func TestVersionsInRangeSkipsServedAndDeleted(t *testing.T) {
	got := nums(VersionsInRange(history(10, 4), 6, 3, 7))
	if want := []int{7, 5, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestVersionsToPrune(t *testing.T) {
	// Unpinned: v10 serves; keep 3 newest others (9, 8, 6 since 7 is gone).
	got := nums(VersionsToPrune(history(10, 7), 10, 3))
	if want := []int{5, 4, 3, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Pinned to an old version: it survives whatever keep says.
	got = nums(VersionsToPrune(history(6), 2, 1))
	if want := []int{5, 4, 3, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned: got %v, want %v", got, want)
	}
	// keep 0 leaves only the served version.
	got = nums(VersionsToPrune(history(3), 3, 0))
	if want := []int{2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keep 0: got %v, want %v", got, want)
	}
}

func TestQuotaNearlyFull(t *testing.T) {
	for _, c := range []struct {
		used, quota int
		want        bool
	}{{80, 100, false}, {81, 100, true}, {5, 0, false}, {900 << 20, 1 << 30, true}} {
		if got := QuotaNearlyFull(c.used, c.quota); got != c.want {
			t.Fatalf("QuotaNearlyFull(%d, %d) = %v", c.used, c.quota, got)
		}
	}
}
