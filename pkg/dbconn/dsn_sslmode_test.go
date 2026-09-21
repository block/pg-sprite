package dbconn

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDSNNamesSSLMode(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want bool
	}{
		{"URL without a query", "postgres://user@db.example:5432/app", false},
		{"URL with sslmode", "postgres://user@db.example:5432/app?sslmode=verify-full", true},
		{"URL with sslmode after another parameter", "postgresql://user@db.example/app?application_name=x&sslmode=disable", true},
		{"URL whose other parameter mentions sslmode in its value", "postgres://user@db.example/app?application_name=sslmode%3Dx", false},
		{"URL with sslrootcert but no sslmode", "postgres://user@db.example/app?sslrootcert=/tmp/ca.pem", false},
		// pgx reads an empty value as its default, so nothing was selected.
		{"URL with an empty sslmode value", "postgres://user@db.example/app?sslmode=", false},
		{"URL with a bare sslmode key", "postgres://user@db.example/app?sslmode", false},
		{"keyword form without sslmode", "host=db.example user=app dbname=app", false},
		{"keyword form with sslmode", "host=db.example sslmode=require dbname=app", true},
		{"keyword form with whitespace around the equals sign", "host=db.example sslmode = disable", true},
		{"keyword form with sslmode as the first keyword", "sslmode=verify-ca host=db.example", true},
		{"keyword form with a quoted sslmode value", "host=db.example sslmode='disable' user=app", true},
		{"keyword form with sslmode as the last keyword and no value", "host=db.example sslmode=", false},
		{"keyword form with a quoted empty sslmode value", "host=db.example sslmode='' user=app", false},
		{"keyword form whose key merely ends in sslmode", "host=db.example xsslmode=disable", false},
		{"keyword form with sslrootcert but no sslmode", "host=db.example sslrootcert=/tmp/ca.pem", false},
		// A quoted value may contain whitespace; what follows the space is
		// still the password, not a keyword.
		{"keyword form whose quoted password contains a spaced sslmode=", "host=db.example password='hunter2 sslmode=off' user=app", false},
		{"keyword form whose quoted password contains an escaped quote before sslmode=", `host=db.example password='a\'b sslmode=off' user=app`, false},
		// A backslash escapes the following space in an unquoted value.
		{"keyword form whose unquoted password escapes a space before sslmode=", `host=db.example password=a\ sslmode=off user=app`, false},
		{"keyword form whose unquoted password contains sslmode= with no space", "host=db.example password=hunter2sslmode=off user=app", false},
		// pgx does not fold key case, so an uppercased key is not sslmode to it either.
		{"keyword form with an uppercased key", "host=db.example SSLMODE=disable", false},
		// The last binding wins, as it does in pgx's settings map.
		{"keyword form binding sslmode twice with the last one empty", "sslmode=disable host=db.example sslmode=''", false},
		{"keyword form with an unterminated quote", "host=db.example password='oops sslmode=disable", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dsnNamesSSLMode(tt.dsn))
		})
	}
}

// TestKeywordDSNSettingAgreesWithPGX pins the scanner to the grammar it
// copies: for every keyword/value string pgx accepts, the password the
// scanner reads is the password pgx reads, so a value the scanner steps over
// is exactly the value pgx would have bound.
func TestKeywordDSNSettingAgreesWithPGX(t *testing.T) {
	for _, dsn := range []string{
		"host=db.example password=hunter2 user=app",
		"host=db.example password='hunter2 sslmode=off' user=app",
		`host=db.example password='it\'s sslmode=off' user=app`,
		`host=db.example password='back\\slash' user=app`,
		`host=db.example password=a\ b user=app`,
		`host=db.example password=a\'b user=app`,
		"host=db.example password = spaced user=app",
		"  host=db.example\tpassword=tabbed\nuser=app  ",
		"host=db.example password='' user=app",
		"host=db.example password=",
	} {
		t.Run(dsn, func(t *testing.T) {
			want, err := pgconn.ParseConfig(dsn)
			require.NoError(t, err)
			assert.Equal(t, want.Password, keywordDSNSetting(dsn, "password"))
			assert.Equal(t, want.Host, keywordDSNSetting(dsn, "host"))
		})
	}
}
