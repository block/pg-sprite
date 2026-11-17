package dbconn

import "strings"

// dsnNamesSSLMode reports whether dsn selects an sslmode, in either
// connection-string form pgx accepts: a URL (postgres:// or postgresql://)
// whose query leaves `sslmode` bound to a non-empty value (directly, or
// through the `ssl=true` alias pgx rewrites to sslmode=require), or a
// keyword/value string binding the `sslmode` keyword to a non-empty value.
// It reads the string without altering it.
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
// libpq grammars copied here, so a string rejected here never produces the
// parsed config configureTLS is called with.
func dsnNamesSSLMode(dsn string) bool {
	return dsnSSLMode(dsn) != ""
}

// dsnSSLMode returns the sslmode dsn selects, or "" when it selects none or
// does not parse. dsnNamesSSLMode explains why the string, not the parsed
// config, is the source.
func dsnSSLMode(dsn string) string {
	if isURLForm(dsn) {
		return urlDSNSSLMode(dsn)
	}
	return keywordDSNSetting(dsn, "sslmode")
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
// The scan follows parseKeywordValueSettings in pgx v5.12.0's
// pgconn/config.go, which matches libpq's keyword/value grammar (copied
// rather than imported: pgx does not export it). A NUL byte anywhere fails
// the string. A key is read up to `=` and trimmed of surrounding whitespace;
// an empty key or one with whitespace inside it fails the string. A value is
// either a single-quoted string, which may contain whitespace and fails the
// string when unterminated, or a run of non-whitespace characters. In both, a
// backslash is dropped and the character after it is taken literally,
// whitespace and quotes included; a backslash that ends an unquoted value is
// dropped and ends it. Stepping over quoted and escaped values is
// what keeps `password='hunter2 sslmode=off'` from reading as an sslmode.
func keywordDSNSetting(dsn, key string) string {
	const space = " \t\n\r\v\f"
	if strings.IndexByte(dsn, 0) >= 0 {
		return ""
	}
	value := ""
	s := strings.TrimLeft(dsn, space)
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return ""
		}
		k := strings.Trim(s[:eq], space)
		if !isKeywordName(k) {
			return ""
		}
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

// isKeywordName reports whether k, already trimmed, is a key libpq's grammar
// accepts: it reads a key as one run of non-whitespace characters, so an
// empty key, or whitespace inside one (`pass word=x`), fails the string.
func isKeywordName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if isKeywordSpace(k[i]) {
			return false
		}
	}
	return true
}

// scanKeywordValue reads one value from the head of s and returns it with the
// unconsumed remainder. An empty s is a legal empty value. ok is false for an
// unterminated quote, which pgx rejects.
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
			// A backslash at the end of the string escapes the end:
			// the value ends there.
			if end == len(s) {
				break
			}
		}
	}
	return unescapeKeywordValue(s[:end]), s[end:], true
}

// scanQuotedKeywordValue reads a single-quoted value whose opening quote has
// already been consumed, honouring backslash escapes inside the quotes. A
// backslash before the end of the string escapes the end, which leaves the
// quote unterminated.
func scanQuotedKeywordValue(s string) (value, rest string, ok bool) {
	end := 0
	for ; end < len(s); end++ {
		if s[end] == '\'' {
			break
		}
		if s[end] == '\\' {
			end++
			if end == len(s) {
				return "", "", false
			}
		}
	}
	if end == len(s) {
		return "", "", false
	}
	return unescapeKeywordValue(s[:end]), s[end+1:], true
}

// unescapeKeywordValue applies libpq's backslash rule to the raw text of a
// value: each backslash is dropped and the character after it is kept
// literally, whatever it is; a backslash at the very end contributes nothing.
func unescapeKeywordValue(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' {
			i++
			if i == len(v) {
				break
			}
		}
		sb.WriteByte(v[i])
	}
	return sb.String()
}

// isKeywordSpace reports whether b is whitespace in libpq's keyword grammar.
func isKeywordSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}
