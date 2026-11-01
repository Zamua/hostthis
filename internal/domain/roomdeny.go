package domain

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

// RoomDenyRule refuses a room value that matches it (docs/SPEC.md "Room value
// rules"). The rules are deployment configuration, not code: a pattern over
// the value read as UTF-8, or a byte prefix.
type RoomDenyRule struct {
	Rule    string
	Message string
	pattern *regexp.Regexp
	prefix  []byte
}

// RoomDenyList is an ordered rule list; the zero value refuses nothing.
type RoomDenyList []RoomDenyRule

// ParseRoomDenyList reads the JSON form:
// [{"rule", "pattern", "flags": "i", "prefixHex", "message"}], each rule with
// a pattern or a prefixHex. Patterns must be valid for both RE2 and
// ECMAScript, since the Worker applies the same list.
func ParseRoomDenyList(raw string) (RoomDenyList, error) {
	if raw == "" {
		return nil, nil
	}
	var specs []struct {
		Rule      string `json:"rule"`
		Pattern   string `json:"pattern"`
		Flags     string `json:"flags"`
		PrefixHex string `json:"prefixHex"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil, fmt.Errorf("room deny list: %w", err)
	}
	out := make(RoomDenyList, 0, len(specs))
	for _, s := range specs {
		r := RoomDenyRule{Rule: s.Rule, Message: s.Message}
		switch {
		case s.Rule == "":
			return nil, fmt.Errorf("room deny list: a rule has no name")
		case s.Pattern != "" && s.PrefixHex == "":
			expr := s.Pattern
			if s.Flags == "i" {
				expr = "(?i)" + expr
			} else if s.Flags != "" {
				return nil, fmt.Errorf("room deny list: %s: only the i flag is supported", s.Rule)
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("room deny list: %s: %w", s.Rule, err)
			}
			r.pattern = re
		case s.PrefixHex != "" && s.Pattern == "":
			b, err := hex.DecodeString(s.PrefixHex)
			if err != nil || len(b) == 0 {
				return nil, fmt.Errorf("room deny list: %s: bad prefixHex", s.Rule)
			}
			r.prefix = b
		default:
			return nil, fmt.Errorf("room deny list: %s: exactly one of pattern and prefixHex", s.Rule)
		}
		out = append(out, r)
	}
	return out, nil
}

// Match returns the first rule val matches.
func (l RoomDenyList) Match(val []byte) (RoomDenyRule, bool) {
	for _, r := range l {
		if r.prefix != nil && len(val) >= len(r.prefix) && string(val[:len(r.prefix)]) == string(r.prefix) {
			return r, true
		}
		if r.pattern != nil && r.pattern.Match(val) {
			return r, true
		}
	}
	return RoomDenyRule{}, false
}
