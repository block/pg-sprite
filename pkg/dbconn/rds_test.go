package dbconn

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsRDSHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"mydb.abc123.us-east-1.rds.amazonaws.com", true},
		{"mydb.cluster-abc123.us-west-2.rds.amazonaws.com", true},
		{"mydb.abc123.eu-west-1.rds.amazonaws.com:5432", true},
		// DNS is case-insensitive, so a console copy-paste is the same host.
		{"MYDB.ABC123.US-EAST-1.RDS.AMAZONAWS.COM", true},
		{"mydb.abc123.us-east-1.RDS.amazonaws.com:5432", true},
		// GovCloud endpoints are RDS endpoints; whether the embedded bundle
		// can verify them is RDSBundleCovers's question, not this one's.
		{"mydb.abc123.us-gov-west-1.rds.amazonaws.com", true},
		{"mydb.cluster-abc123.us-gov-east-1.rds.amazonaws.com:5432", true},
		// China endpoints carry a different suffix.
		{"mydb.abc123.cn-north-1.rds.amazonaws.com.cn", false},
		{"fake-rds.amazonaws.com", false},
		{"rds.amazonaws.com", false},
		{"mydb.rds.amazonaws.com.evil.example", false},
		{"localhost", false},
		{"127.0.0.1", false},
		{"db.internal.example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.want, IsRDSHost(tt.host))
		})
	}
}

func TestRDSBundleCovers(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"mydb.abc123.us-east-1.rds.amazonaws.com", true},
		{"mydb.abc123.eu-west-1.rds.amazonaws.com:5432", true},
		{"MYDB.ABC123.US-EAST-1.RDS.AMAZONAWS.COM", true},
		// The embedded bundle carries no GovCloud roots, in any spelling.
		{"mydb.abc123.us-gov-west-1.rds.amazonaws.com", false},
		{"mydb.cluster-abc123.us-gov-east-1.rds.amazonaws.com:5432", false},
		{"MYDB.ABC123.US-GOV-WEST-1.RDS.AMAZONAWS.COM", false},
		// Not RDS at all, so nothing to cover.
		{"mydb.abc123.cn-north-1.rds.amazonaws.com.cn", false},
		{"fake-rds.amazonaws.com", false},
		{"localhost", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.want, RDSBundleCovers(tt.host))
		})
	}
}

func TestEmbeddedRDSBundleParses(t *testing.T) {
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(rdsGlobalBundle),
		"embedded RDS bundle must contain usable certificates")
}

// TestEmbeddedRDSBundleCoversOnlyCommercialRegions pins the premise behind
// RDSBundleCovers: every root in the embedded bundle names a commercial
// region, so excluding GovCloud reflects what the bundle can verify. A bundle
// refresh that adds GovCloud roots, or drops the commercial ones, fails here
// instead of on someone's connection.
func TestEmbeddedRDSBundleCoversOnlyCommercialRegions(t *testing.T) {
	rootName := regexp.MustCompile(`^Amazon RDS (?:Preview |Beta )?([a-z]+-[a-z]+-\d+) Root CA `)
	regions := map[string]bool{}
	for block, rest := pem.Decode(rdsGlobalBundle); block != nil; block, rest = pem.Decode(rest) {
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		m := rootName.FindStringSubmatch(cert.Subject.CommonName)
		require.Len(t, m, 2, "root %q does not name a region", cert.Subject.CommonName)
		regions[m[1]] = true
	}
	assert.True(t, regions["us-east-1"], "the bundle must carry the commercial region roots it is relied on for")
	for region := range regions {
		assert.False(t, govCloudHostPattern.MatchString("db.x."+region+".rds.amazonaws.com"),
			"RDSBundleCovers excludes GovCloud because the bundle has no roots for it; region %q says otherwise", region)
		assert.NotContains(t, region, "cn-", "China roots would need their own suffix handling")
	}
}

func TestRDSTLSConfig(t *testing.T) {
	tc := rdsTLSConfig("mydb.abc123.us-east-1.rds.amazonaws.com")
	assert.Equal(t, "mydb.abc123.us-east-1.rds.amazonaws.com", tc.ServerName)
	assert.NotNil(t, tc.RootCAs)
	assert.Equal(t, uint16(tls.VersionTLS12), tc.MinVersion)
	assert.False(t, tc.InsecureSkipVerify)
}

func TestConfigureTLS(t *testing.T) {
	const rdsURL = "postgres://user@mydb.abc123.us-east-1.rds.amazonaws.com:5432/app"
	const govCloudURL = "postgres://user@mydb.abc123.us-gov-west-1.rds.amazonaws.com:5432/app"

	parse := func(t *testing.T, url string) *pgxpool.Config {
		t.Helper()
		pc, err := pgxpool.ParseConfig(url)
		require.NoError(t, err)
		return pc
	}
	// assertAutoTLS checks the shape auto-TLS leaves behind: verify-full
	// against the embedded roots with no plaintext fallback.
	assertAutoTLS := func(t *testing.T, pc *pgxpool.Config) {
		t.Helper()
		require.NotNil(t, pc.ConnConfig.TLSConfig)
		assert.NotNil(t, pc.ConnConfig.TLSConfig.RootCAs)
		assert.False(t, pc.ConnConfig.TLSConfig.InsecureSkipVerify)
		assert.Nil(t, pc.ConnConfig.Fallbacks, "plaintext fallbacks must be dropped for RDS hosts")
	}

	t.Run("RDS host without sslmode gets verify-full with embedded roots and no plaintext fallback", func(t *testing.T) {
		pc := parse(t, rdsURL)
		require.NoError(t, configureTLS(pc, Config{URL: rdsURL}))
		assertAutoTLS(t, pc)
		assert.Equal(t, "mydb.abc123.us-east-1.rds.amazonaws.com", pc.ConnConfig.TLSConfig.ServerName)
	})

	t.Run("RDS host with explicit sslmode=disable is honored", func(t *testing.T) {
		url := rdsURL + "?sslmode=disable"
		pc := parse(t, url)
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		assert.Nil(t, pc.ConnConfig.TLSConfig)
	})

	t.Run("RDS host with an empty sslmode value is treated as choosing nothing and gets auto-TLS", func(t *testing.T) {
		// pgx reads `?sslmode=` as its default (prefer), so nothing was
		// selected; a templated `?sslmode=${PGSSLMODE}` with the variable
		// unset must not silently lose verification.
		url := rdsURL + "?sslmode="
		pc := parse(t, url)
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		assertAutoTLS(t, pc)
	})

	t.Run("RDS host in a keyword DSN with whitespace around sslmode's equals sign is honored", func(t *testing.T) {
		dsn := "host=mydb.abc123.us-east-1.rds.amazonaws.com port=5432 user=user dbname=app sslmode = disable"
		pc := parse(t, dsn)
		require.NoError(t, configureTLS(pc, Config{URL: dsn}))
		assert.Nil(t, pc.ConnConfig.TLSConfig,
			"a spaced sslmode keyword is an explicit choice and must not be replaced by auto-TLS")
	})

	t.Run("RDS host in a keyword DSN whose quoted password contains sslmode= still gets auto-TLS", func(t *testing.T) {
		dsn := "host=mydb.abc123.us-east-1.rds.amazonaws.com user=user password='hunter2 sslmode=off' dbname=app"
		pc := parse(t, dsn)
		require.Equal(t, "hunter2 sslmode=off", pc.ConnConfig.Password, "pgx reads the quoted password whole")
		require.NoError(t, configureTLS(pc, Config{URL: dsn}))
		assertAutoTLS(t, pc)
	})

	t.Run("uppercased RDS host without sslmode gets verify-full like its lowercase spelling", func(t *testing.T) {
		url := "postgres://user@MYDB.ABC123.US-EAST-1.RDS.AMAZONAWS.COM:5432/app"
		pc := parse(t, url)
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		assertAutoTLS(t, pc)
	})

	t.Run("GovCloud RDS host without sslmode is refused because the embedded bundle cannot verify it", func(t *testing.T) {
		pc := parse(t, govCloudURL)
		require.ErrorIs(t, configureTLS(pc, Config{URL: govCloudURL}), ErrRDSBundleDoesNotCoverHost)
	})

	t.Run("GovCloud RDS host asking for verify-full without roots is refused rather than left to fail the handshake", func(t *testing.T) {
		url := govCloudURL + "?sslmode=verify-full"
		pc := parse(t, url)
		require.ErrorIs(t, configureTLS(pc, Config{URL: url}), ErrRDSBundleDoesNotCoverHost)
	})

	t.Run("GovCloud RDS host with an explicit sslmode that needs no roots is honored", func(t *testing.T) {
		url := govCloudURL + "?sslmode=require"
		pc := parse(t, url)
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		require.NotNil(t, pc.ConnConfig.TLSConfig)
		assert.True(t, pc.ConnConfig.TLSConfig.InsecureSkipVerify, "require is encrypted but unverified, as the caller chose")
		assert.Nil(t, pc.ConnConfig.TLSConfig.RootCAs, "no roots are injected for a mode that does not verify")
	})

	t.Run("GovCloud RDS host with CACertPath gets verify-full against that bundle", func(t *testing.T) {
		caPath := filepath.Join(t.TempDir(), "partition-bundle.pem")
		require.NoError(t, os.WriteFile(caPath, rdsGlobalBundle, 0o600))
		pc := parse(t, govCloudURL)
		require.NoError(t, configureTLS(pc, Config{URL: govCloudURL, CACertPath: caPath}))
		require.NotNil(t, pc.ConnConfig.TLSConfig)
		assert.Equal(t, "mydb.abc123.us-gov-west-1.rds.amazonaws.com", pc.ConnConfig.TLSConfig.ServerName)
		assert.NotNil(t, pc.ConnConfig.TLSConfig.RootCAs)
		assert.False(t, pc.ConnConfig.TLSConfig.InsecureSkipVerify)
	})

	t.Run("RDS host with sslmode=verify-full gets the embedded roots injected", func(t *testing.T) {
		url := rdsURL + "?sslmode=verify-full"
		pc := parse(t, url)
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		require.NotNil(t, pc.ConnConfig.TLSConfig)
		assert.NotNil(t, pc.ConnConfig.TLSConfig.RootCAs,
			"verification without a bundle must get the embedded RDS roots")
	})

	t.Run("non-RDS host is left untouched", func(t *testing.T) {
		url := "postgres://user@localhost:5432/app"
		pc := parse(t, url)
		before := pc.ConnConfig.TLSConfig
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		assert.Equal(t, before, pc.ConnConfig.TLSConfig)
	})
}
