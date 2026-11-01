package domain

import "testing"

const testRoomDeny = `[
 {"rule":"pay","pattern":"ko-fi\\.com/","flags":"i","message":"personal use only"},
 {"rule":"exe","prefixHex":"4d5a"}
]`

func TestRoomDenyList_MatchesPatternsAndPrefixes(t *testing.T) {
	l, err := ParseRoomDenyList(testRoomDeny)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"tip":"https://KO-FI.com/me"}`: "pay",
		"MZ\x90\x00binary":               "exe",
		`{"score":3}`:                    "",
		"xMZ":                            "",
	}
	for val, want := range cases {
		r, ok := l.Match([]byte(val))
		if got := map[bool]string{true: r.Rule, false: ""}[ok]; got != want {
			t.Errorf("Match(%q) = %q, want %q", val, got, want)
		}
	}
	if r, _ := l.Match([]byte("ko-fi.com/x")); r.Message != "personal use only" {
		t.Errorf("message %q", r.Message)
	}
}

func TestRoomDenyList_RefusesMalformedConfig(t *testing.T) {
	for _, raw := range []string{
		`not json`, `[{"pattern":"x"}]`, `[{"rule":"a"}]`, `[{"rule":"a","pattern":"(","prefixHex":""}]`,
		`[{"rule":"a","pattern":"x","prefixHex":"00"}]`, `[{"rule":"a","prefixHex":"zz"}]`, `[{"rule":"a","pattern":"x","flags":"g"}]`,
	} {
		if _, err := ParseRoomDenyList(raw); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
	if l, err := ParseRoomDenyList(""); err != nil || l != nil {
		t.Errorf("empty: %v %v", l, err)
	}
}
