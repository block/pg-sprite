package dbconn

import (
	"net/url"
	"strings"
)

// dsnNamesSSLMode reports whether dsn selects an sslmode, in either
// connection-string form pgx accepts: a URL (postgres:// or postgresql://)
// carrying a non-empty `sslmode` in its query, or a keyword/value string
// binding the `sslmode` keyword to a non-empty value. It reads the string
// without altering it.
//
// pgx folds an absent sslmode into its default before configureTLS sees the
// parsed config, so the parsed config cannot say whether the caller chose the
// mode or inherited it; only the string can. pgx also treats an empty value
// (`?sslmode=`, or the keyword bound to an empty quoted string) exactly like
// an absent one, so the question is "was a mode selected", not "does the
// string mention sslmode".
//
// A string that does not parse reports false. That branch is unreachable
// from configureTLS rather than defensive: pgx parses both forms with the
// same grammar (net/url for URLs, libpq's keyword/value rules otherwise), so
// a string rejected here never produces the parsed config configureTLS is
// called with.
func dsnNamesSSLMode(dsn string) bool {
	if isURLForm(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return false
		}
		return u.Query().Get("sslmode") != ""
	}
	return keywordDSNSetting(dsn, "sslmode") != ""
}

// isURLForm reports whether dsn is a connection URL rather than a
// keyword/value string; the two forms carry sslmode in different places. The
// two case-sensitive prefixes are the ones pgx itself branches on, so this
// and pgx always agree on which grammar a string is in.
func isURLForm(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// keywordDSNSetting returns the value bound to key in a libpq keyword/value
// connection string, or "" when the key is absent, bound to an empty value,
// or the string does not parse. The last binding wins, as it does for pgx.
//
// The scan follows the grammar pgx's pgconn.ParseConfig implements for this
// form (copied rather than imported: pgx does not export it): keys are read
// up to `=` and trimmed of surrounding whitespace; a value is either a
// single-quoted string that may contain whitespace and backslash-escaped
// characters, or a run of non-whitespace characters in which a backslash
// escapes the next character. Stepping over quoted and escaped values is
// what keeps `password='hunter2 sslmode=off'` from reading as an sslmode.
func keywordDSNSetting(dsn, key string) string {
	const space = " \t\n\r\v\f"
	value := ""
	s := strings.TrimLeft(dsn, space)
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return ""
		}
		k := strings.Trim(s[:eq], space)
		s = strings.TrimLeft(s[eq+1:], space)
		v, rest, ok := scanKeywordValue(s)
		if !ok {
			return ""
		}
		if k == key {
			value = v
		}
		s = strings.TrimLeft(rest, space)
	}
	return value
}

// scanKeywordValue reads one value from the head of s and returns it with the
// unconsumed remainder. An empty s is a legal empty value. ok is false for an
// unterminated quote or a trailing backslash, which pgx rejects.
func scanKeywordValue(s string) (value, rest string, ok bool) {
	if len(s) == 0 {
		return "", "", true
	}
	if s[0] == '\'' {
		return scanQuotedKeywordValue(s[1:])
	}
	end := 0
	for ; end < len(s); end++ {
		if isKeywordSpace(s[end]) {
			break
		}
		if s[end] == '\\' {
			end++
			if end == len(s) {
				return "", "", false
			}
		}
	}
	return unescapeKeywordValue(s[:end]), s[end:], true
}

// scanQuotedKeywordValue reads a single-quoted value whose opening quote has
// already been consumed, honouring backslash escapes inside the quotes.
func scanQuotedKeywordValue(s string) (value, rest string, ok bool) {
	end := 0
	for ; end < len(s); end++ {
		if s[end] == '\'' {
			break
		}
		if s[end] == '\\' {
			end++
		}
	}
	if end >= len(s) {
		return "", "", false
	}
	return unescapeKeywordValue(s[:end]), s[end+1:], true
}

// unescapeKeywordValue resolves the two escapes libpq's grammar defines.
func unescapeKeywordValue(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, `\\`, `\`), `\'`, `'`)
}

// isKeywordSpace reports whether b is whitespace in libpq's keyword grammar.
func isKeywordSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}
