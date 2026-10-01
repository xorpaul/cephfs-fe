// Copied from cephfs-index internal/index/search.go (NamePrefix,
// RequiredLiterals, UpperBound). Keep in sync with that file.

package pgsearch

import "regexp/syntax"

// NamePrefix returns the literal every matching name must start with, or "".
// Only patterns anchored with ^ qualify: regexp.LiteralPrefix also reports a
// prefix for unanchored patterns, whose matches may start mid-name.
func NamePrefix(pattern string) string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return ""
	}
	re = re.Simplify()
	if re.Op != syntax.OpConcat || len(re.Sub) < 2 {
		return ""
	}
	if op := re.Sub[0].Op; op != syntax.OpBeginText && op != syntax.OpBeginLine {
		return ""
	}
	lit := re.Sub[1]
	if lit.Op != syntax.OpLiteral || lit.Flags&syntax.FoldCase != 0 {
		return ""
	}
	return string(lit.Rune)
}

// RequiredLiterals returns literal substrings (at least minLit bytes) that
// every match of pattern must contain. A backend can pre-filter on them with
// a plain substring test and then re-check the full regex: the filter is
// always a superset, whatever the backend's own regex dialect. Case-folded
// literals and alternations contribute nothing.
func RequiredLiterals(pattern string) []string {
	const minLit = 2
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	var out []string
	var walk func(*syntax.Regexp)
	walk = func(r *syntax.Regexp) {
		switch r.Op {
		case syntax.OpLiteral:
			if r.Flags&syntax.FoldCase == 0 && len(string(r.Rune)) >= minLit {
				out = append(out, string(r.Rune))
			}
		case syntax.OpConcat:
			for _, sub := range r.Sub {
				walk(sub)
			}
		case syntax.OpCapture, syntax.OpPlus:
			walk(r.Sub[0])
		case syntax.OpRepeat:
			if r.Min >= 1 {
				walk(r.Sub[0])
			}
		}
	}
	walk(re.Simplify())
	return out
}

// UpperBound returns the smallest string greater than every string with
// prefix p, or false if there is none (p is all 0xff bytes).
func UpperBound(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// Literal kinds for PureLiteral.
const (
	NotPure  = iota
	Exact    // ^lit$
	Prefix   // ^lit
	Contains // lit
)

// PureLiteral reports whether pattern is nothing but a case-sensitive
// literal, optionally anchored at the start (^lit) or at both ends (^lit$).
// For such patterns the SQL filter (name = lit, the btree range of the
// prefix, or strpos) selects exactly the matching names, so no row needs a
// regex re-check and the query can stop after LIMIT rows.
func PureLiteral(pattern string) (kind int, lit string) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return NotPure, ""
	}
	re = re.Simplify()
	isLit := func(r *syntax.Regexp) bool {
		return r.Op == syntax.OpLiteral && r.Flags&syntax.FoldCase == 0
	}
	switch {
	case isLit(re):
		return Contains, string(re.Rune)
	case re.Op != syntax.OpConcat || len(re.Sub) < 2 || re.Sub[0].Op != syntax.OpBeginText || !isLit(re.Sub[1]):
		return NotPure, ""
	case len(re.Sub) == 2:
		return Prefix, string(re.Sub[1].Rune)
	case len(re.Sub) == 3 && re.Sub[2].Op == syntax.OpEndText:
		return Exact, string(re.Sub[1].Rune)
	}
	return NotPure, ""
}
