package ssh

import "testing"

func TestCommandFieldsNamesOnlyTheVerbAndSlug(t *testing.T) {
	cases := []struct {
		argv       []string
		verb, slug string
	}{
		{nil, "upload", "-"},
		{[]string{"--name", "abcd2345"}, "upload", "-"},
		{[]string{"abcd2345"}, "update", "abcd2345"},
		{[]string{"abcd2345", "--name", "label"}, "update", "abcd2345"},
		{[]string{"versions", "abcd2345", "-o", "json"}, "versions", "abcd2345"},
		{[]string{"delete", "abcd2345", "3"}, "delete", "abcd2345"},
		{[]string{"versions"}, "versions", "-"},
		{[]string{"list", "-o", "json"}, "list", "-"},
		{[]string{"help", "versions"}, "help", "-"},
		{[]string{"rename", "not-a-slug!", "x"}, "rename", "-"},
		{[]string{"drop table"}, "unknown", "-"},
	}
	for _, tc := range cases {
		verb, slug := commandFields(tc.argv)
		if verb != tc.verb || slug != tc.slug {
			t.Errorf("commandFields(%q) = (%q, %q), want (%q, %q)", tc.argv, verb, slug, tc.verb, tc.slug)
		}
	}
}
