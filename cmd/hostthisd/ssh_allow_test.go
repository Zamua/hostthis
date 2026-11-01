package main

import (
	"strings"
	"testing"
)

func TestSSHAllowList_EmptyIsNoneAndATypoIsAnError(t *testing.T) {
	if got, err := sshAllowList(" , "); got != nil || err != nil {
		t.Fatalf("empty: %v, %v; want no list", got, err)
	}
	key := "SHA256:" + strings.Repeat("ab", 32)
	got, err := sshAllowList(key + ", 203.0.113.0/24")
	if err != nil || got.Len() != 2 {
		t.Fatalf("valid: %v, %v", got, err)
	}
	if _, err := sshAllowList(key + ",203.0.113.0"); err == nil {
		t.Fatal("an entry without a prefix length was accepted")
	}
}
