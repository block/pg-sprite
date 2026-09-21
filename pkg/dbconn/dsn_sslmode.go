package dbconn

import (
	"net/url"
	"regexp"
	"strings"
)

// keywordSSLModePattern matches an sslmode keyword in a keyword/value
// connection string. libpq's keyword parser accepts whitespace around the
// `=`, so `sslmode = disable` is a legal, explicit setting and must be
// recognized as one. Anchoring on the start of the string or preceding
// whitespace keeps a key that merely ends in "sslmode" from matching.
var keywordSSLModePattern = regexp.MustCompile(`(^|\s)sslmode\s*=`)

// dsnNamesSSLMode reports whether dsn spells out an sslmode, in either
// connection-string form pgx accepts: a URL (postgres:// or postgresql://)
// carrying `sslmode` in its query, or a keyword/value string carrying an
// `sslmode` keyword. It reads the string without altering it.
//
// pgx folds an absent sslmode into its default before configureTLS sees the
// parsed config, so the parsed config cannot say whether the caller chose the
// mode or inherited it; only the string can. A URL that does not parse
// reports false, leaving pgx to report the parse failure itself.
func dsnNamesSSLMode(dsn string) bool {
	if isURLForm(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return false
		}
		return u.Query().Has("sslmode")
	}
	return keywordSSLModePattern.MatchString(dsn)
}

// isURLForm reports whether dsn is a connection URL rather than a
// keyword/value string; the two forms carry sslmode in different places.
func isURLForm(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}
