// Package sqlident is the single implementation of the slice of SQLite's
// identifier grammar this project needs to recognize consistently in more
// than one place (internal/schemadiff's tokenizer, internal/sqldefwrap's
// sqldef-output parser): an unquoted run of identifier characters, or a
// double-quoted identifier with SQLite's doubled-quote escaping for an
// embedded quote.
package sqlident

import "strings"

// IsIdentByte reports whether c can appear in an unquoted SQL identifier.
func IsIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// QuoteIdent double-quote-wraps a SQL identifier, doubling any embedded
// double quote, so an identifier from schema.sql is never string-concatenated
// raw into generated SQL.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ScanQuoted returns the index just past the end of a quoted run starting
// at s[start] (which holds the opening quote char), honoring the SQL
// convention that a doubled quote char is an escaped literal quote inside
// the run rather than its terminator.
func ScanQuoted(s string, start int, quote byte) int {
	n := len(s)
	j := start + 1
	for j < n {
		if s[j] == quote {
			if j+1 < n && s[j+1] == quote {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return n
}

// Unquote strips a quoted run's opening/closing quote chars and collapses
// any doubled quote char inside it back to one.
func Unquote(s string, quote byte) string {
	inner := s
	if len(inner) >= 2 && inner[0] == quote && inner[len(inner)-1] == quote {
		inner = inner[1 : len(inner)-1]
	}
	return strings.ReplaceAll(inner, string(quote)+string(quote), string(quote))
}

// ScanIdent scans one SQL identifier starting at s[start]: either an
// unquoted run of identifier bytes, or a double-quoted identifier. It
// returns the identifier's raw text (still quoted, if it was one) and the
// index just past it. ok is false if s[start] does not begin an
// identifier, in which case token and end are zero values.
func ScanIdent(s string, start int) (token string, end int, ok bool) {
	if start >= len(s) {
		return "", start, false
	}
	if s[start] == '"' {
		end = ScanQuoted(s, start, '"')
		return s[start:end], end, true
	}
	if IsIdentByte(s[start]) {
		end = start
		for end < len(s) && IsIdentByte(s[end]) {
			end++
		}
		return s[start:end], end, true
	}
	return "", start, false
}
