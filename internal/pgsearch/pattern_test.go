package pgsearch

import (
	"regexp"
	"strings"
	"testing"
)

func TestNamePrefix(t *testing.T) {
	for pattern, want := range map[string]string{
		"^plugins$":      "plugins",
		`^settings\.db$`: "settings.db",
		"^tmp_":          "tmp_",
		"plugins":        "", // unanchored: may match mid-name
		"(?i)^abc":       "",
		"^a|^b":          "",
		"^(abc)":         "",
		"^":              "",
	} {
		if got := NamePrefix(pattern); got != want {
			t.Errorf("NamePrefix(%q) = %q, want %q", pattern, got, want)
		}
	}
	if hi, ok := UpperBound("ab\xff"); !ok || hi != "ac" {
		t.Errorf("UpperBound = %q %v", hi, ok)
	}
}

func TestRequiredLiterals(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{`app-config\.php`, []string{"app-config.php"}},
		{`^plugins$`, []string{"plugins"}},
		{`settings\.db$`, []string{"settings.db"}},
		{`tmp_[0-9a-z]+\.tmp`, []string{"tmp_", ".tmp"}},
		{`foo(bar)+baz`, []string{"foo", "bar", "baz"}},
		{`x?yz.*abc`, []string{"yz", "abc"}},
		{`(?i)app-config`, nil},
		{`foo|bar`, nil},
		{`(ab)*cd`, []string{"cd"}},
		{`[ab]c`, nil},
		{`.`, nil},
		{`.*`, nil},
		{`(`, nil},
		// Literal (non-regex) mode quotes the input; the pre-filter must
		// still see the whole string.
		{regexp.QuoteMeta("index.php"), []string{"index.php"}},
		{regexp.QuoteMeta("a+b (1).txt"), []string{"a+b (1).txt"}},
	} {
		got := RequiredLiterals(tc.pattern)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("RequiredLiterals(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

func TestPureLiteral(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		kind    int
		lit     string
	}{
		{`^backup\.json$`, Exact, "backup.json"},
		{`^backup\.json`, Prefix, "backup.json"},
		{`backup\.json`, Contains, "backup.json"},
		{"^" + regexp.QuoteMeta("a+b (1).txt") + "$", Exact, "a+b (1).txt"},
		{`^a$`, Exact, "a"},
		{`backup\.json$`, NotPure, ""}, // suffix: strpos is a superset
		{`^backup.json$`, NotPure, ""}, // '.' is any char
		{`(?i)^backup$`, NotPure, ""},
		{`^(backup)$`, NotPure, ""},
		{`(?m)^backup$`, NotPure, ""}, // line anchors
		{`^`, NotPure, ""},
		{`.*`, NotPure, ""},
		{`(`, NotPure, ""},
	} {
		kind, lit := PureLiteral(tc.pattern)
		if kind != tc.kind || lit != tc.lit {
			t.Errorf("PureLiteral(%q) = %d %q, want %d %q", tc.pattern, kind, lit, tc.kind, tc.lit)
		}
	}
}
