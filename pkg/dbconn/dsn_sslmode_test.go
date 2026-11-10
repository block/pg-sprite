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
		// A backslash escapes any character and is dropped, so an escaped
		// space keeps what follows inside the password.
		{"keyword form whose unquoted password escapes a space before sslmode=disable", `host=db.example password=x\ sslmode=disable`, false},
		{"keyword form whose sslmode value carries an escaped letter", `host=db.example sslmode=dis\able`, true},
		// A backslash that ends the string ends the value; it is not an error.
		{"keyword form whose sslmode value ends in a backslash", `host=db.example sslmode=require\`, true},
		{"keyword form whose quoted value ends in an escaped quote", `host=db.example sslmode=disable password='oops\`, false},
		{"keyword form with whitespace inside a key", "host=db.example pass word=x sslmode=disable", false},
		{"keyword form with an empty key", "=x sslmode=disable", false},
		{"keyword form with a NUL byte", "host=db.example sslmode=disable\x00", false},
		// The last occurrence of a repeated query key wins.
		{"URL binding sslmode twice with the last one empty", "postgres://user@db.example/app?sslmode=disable&sslmode=", false},
		{"URL binding sslmode twice with the first one empty", "postgres://user@db.example/app?sslmode=&sslmode=disable", true},
		// ssl=true is pgx's alias for sslmode=require.
		{"URL selecting sslmode through ssl=true", "postgres://user@db.example/app?ssl=true", true},
		{"URL with ssl set to something other than true", "postgres://user@db.example/app?ssl=1", false},
		// # is ordinary data, not a fragment, so the query continues past it.
		{"URL with a # before sslmode", "postgres://user@db.example/app?application_name=a#&sslmode=disable", true},
		// The first @ before any / ends the user info, so this URL's host is
		// `b&sslmode=disable` and it has no query.
		{"URL with an @ in the query and no path", "postgres://db.example?application_name=a@b&sslmode=disable", false},
		{"URL with malformed percent-encoding after sslmode", "postgres://user@db.example/app?sslmode=disable&application_name=%zz", false},
		{"URL with a query pair missing its equals sign", "postgres://user@db.example/app?sslmode=disable&application_name", false},
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
		// A backslash escapes any character and is dropped.
		`host=db.example password=a\b user=app`,
		`host=db.example password=a\\b user=app`,
		`host=db.example password='a\b' user=app`,
		`host=db.example password=C:\\certs\\ca.pem user=app`,
		"host=db.example password=a\\\tb user=app",
		`host=db.example password=x\ sslmode=disable`,
		// A backslash that ends the string ends the value and adds nothing.
		`host=db.example password=ab\`,
		`host=db.example password=\`,
	} {
		t.Run(dsn, func(t *testing.T) {
			want, err := pgconn.ParseConfig(dsn)
			require.NoError(t, err)
			assert.Equal(t, want.Password, keywordDSNSetting(dsn, "password"))
			assert.Equal(t, want.Host, keywordDSNSetting(dsn, "host"))
		})
	}
}

// TestDSNSSLModeAgreesWithPGX holds both scanners to the sslmode pgx applies:
// for every string pgx accepts, the mode the scanner reads (pgx's default,
// prefer, when it reads none) is the mode visible in the TLS attempts pgx
// built. Only disable, prefer, and require appear, since pgxSSLMode tells
// exactly those three apart.
func TestDSNSSLModeAgreesWithPGX(t *testing.T) {
	clearSSLEnv(t)
	for _, dsn := range pgxAcceptedDSNs {
		t.Run(dsn, func(t *testing.T) {
			cfg, err := pgconn.ParseConfig(dsn)
			require.NoError(t, err)
			want := pgxSSLMode(t, cfg)
			got := dsnSSLMode(dsn)
			if got == "" {
				got = "prefer"
			}
			assert.Equal(t, want, got)
		})
	}
}

// TestDSNSSLModeRejectedByPGX covers strings pgx refuses to parse. They never
// reach configureTLS, and the scanner reads no sslmode from them, so none can
// keep a selected mode in place of the verified one configureTLS chooses.
func TestDSNSSLModeRejectedByPGX(t *testing.T) {
	clearSSLEnv(t)
	for _, dsn := range pgxRejectedDSNs {
		t.Run(dsn, func(t *testing.T) {
			_, err := pgconn.ParseConfig(dsn)
			require.Error(t, err)
			assert.False(t, dsnNamesSSLMode(dsn))
		})
	}
}

// clearSSLEnv empties the environment variables pgx reads TLS settings from,
// so the connection string alone decides the mode.
func clearSSLEnv(t testing.TB) {
	t.Helper()
	for _, name := range []string{"PGSSLMODE", "PGSSLROOTCERT", "PGSSLNEGOTIATION", "PGSSLCERT", "PGSSLKEY"} {
		t.Setenv(name, "")
	}
}

// pgxSSLMode names the sslmode pgx applied to a single-host TCP config,
// telling disable, prefer, and require apart by the TLS attempts pgx built:
// disable has one plaintext attempt, prefer an unverified TLS attempt with a
// plaintext fallback, and require one unverified TLS attempt.
func pgxSSLMode(t *testing.T, cfg *pgconn.Config) string {
	t.Helper()
	tc := cfg.TLSConfig
	switch {
	case tc == nil:
		require.Empty(t, cfg.Fallbacks, "a plaintext first attempt with fallbacks is sslmode=allow")
		return "disable"
	case len(cfg.Fallbacks) == 1:
		require.Nil(t, cfg.Fallbacks[0].TLSConfig, "prefer falls back to plaintext")
		return "prefer"
	default:
		require.Empty(t, cfg.Fallbacks)
		require.True(t, tc.InsecureSkipVerify, "require skips certificate verification")
		require.Nil(t, tc.VerifyPeerCertificate, "require without sslrootcert verifies no chain")
		return "require"
	}
}

// pgxAcceptedDSNs are strings pgx parses, each naming (or leaving out)
// sslmode in a way the scanners must read as pgx does. They also seed
// FuzzDSNSSLModeAgreesWithPGX.
var pgxAcceptedDSNs = []string{
	"host=db.example",
	"host=db.example\fsslmode=disable",
	"host=db.example sslmode=disable",
	"host=db.example sslmode='require'",
	"sslmode=disable host=db.example sslmode=''",
	`host=db.example password=x\ sslmode=disable`,
	`host=db.example password='x\' sslmode=disable'`,
	`host=db.example sslmode=dis\able`,
	`host=db.example sslmode=require\`,
	`host=db.example sslmode=require password=a\`,
	"postgres://user@db.example/app",
	"postgres://user@db.example/app?sslmode=disable",
	"postgres://user@db.example/app?sslmode=disable&sslmode=",
	"postgres://user@db.example/app?sslmode=&sslmode=disable",
	"postgres://user@db.example/app?sslmode=  disable",
	"postgres://user@db.example/app?ssl=true",
	"postgres://user@db.example/app?ssl=true&sslmode=disable",
	"postgres://user@db.example/app?sslmode=disable&ssl=true",
	"postgres://user@db.example/app?ssl=true&ssl=false",
	"postgres://user@db.example/app?application_name=a#&sslmode=disable",
	"postgres://user@db.example/app?application_name=a+b&sslmode=disable",
	"postgres://db.example?application_name=a@b&sslmode=disable",
	"postgres://us%65r@db.example:5432/app?ss%6Cmode=disable",
	"postgresql://user@db.example/?sslmode=require",
}

// pgxRejectedDSNs are strings pgx refuses to parse, several of them hiding
// an sslmode the scanners must not read. They also seed
// FuzzDSNSSLModeAgreesWithPGX.
var pgxRejectedDSNs = []string{
	"host=db.example password='oops sslmode=disable",
	`host=db.example sslmode=disable password='oops\`,
	"host=db.example pass word=x sslmode=disable",
	"=x sslmode=disable",
	"host=db.example sslmode=disable\x00",
	"host=db.example sslmode=disable password",
	"postgres://user@db.example/app?sslmode",
	"postgres://user@db.example/app?sslmode=disable&application_name",
	"postgres://user@db.example/app?sslmode=disable=x",
	"postgres://user@db.example/app?sslmode=disable&application_name=%zz",
	"postgres://user@db.example/app?sslmode=disable&application_name=%0",
	"postgres://user@db.example/app?sslmode=disable&application_name=%00",
	"postgres://user@db.example/app?sslmode=disable&application_name=a b",
	"postgres://user@db.example/app?sslmode=disable\x00",
	"postgres://us%zzer@db.example/app?sslmode=disable",
	"postgres://user@db.example/a%zzpp?sslmode=disable",
	"postgres://user@db.example:54%zz32/app?sslmode=disable",
	"postgres://db1 ,db2/app?sslmode=disable",
	"postgres://[]/app?sslmode=disable",
	"postgres://[::1/app?sslmode=disable",
	"postgres://[::1]x/app?sslmode=disable",
}
