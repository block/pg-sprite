package dbconn

import (
	"crypto/tls"
	"crypto/x509"
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
		// GovCloud and China endpoints are outside the embedded bundle's roots.
		{"mydb.abc123.us-gov-west-1.rds.amazonaws.com", false},
		{"mydb.cluster-abc123.us-gov-east-1.rds.amazonaws.com:5432", false},
		{"MYDB.ABC123.US-GOV-WEST-1.RDS.AMAZONAWS.COM", false},
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

func TestEmbeddedRDSBundleParses(t *testing.T) {
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(rdsGlobalBundle),
		"embedded RDS bundle must contain usable certificates")
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

	parse := func(t *testing.T, url string) *pgxpool.Config {
		t.Helper()
		pc, err := pgxpool.ParseConfig(url)
		require.NoError(t, err)
		return pc
	}

	t.Run("RDS host without sslmode gets verify-full with embedded roots and no plaintext fallback", func(t *testing.T) {
		pc := parse(t, rdsURL)
		require.NoError(t, configureTLS(pc, Config{URL: rdsURL}))
		require.NotNil(t, pc.ConnConfig.TLSConfig)
		assert.Equal(t, "mydb.abc123.us-east-1.rds.amazonaws.com", pc.ConnConfig.TLSConfig.ServerName)
		assert.NotNil(t, pc.ConnConfig.TLSConfig.RootCAs)
		assert.False(t, pc.ConnConfig.TLSConfig.InsecureSkipVerify)
		assert.Nil(t, pc.ConnConfig.Fallbacks, "plaintext fallbacks must be dropped for RDS hosts")
	})

	t.Run("RDS host with explicit sslmode=disable is honored", func(t *testing.T) {
		url := rdsURL + "?sslmode=disable"
		pc := parse(t, url)
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		assert.Nil(t, pc.ConnConfig.TLSConfig)
	})

	t.Run("RDS host in a keyword DSN with whitespace around sslmode's equals sign is honored", func(t *testing.T) {
		dsn := "host=mydb.abc123.us-east-1.rds.amazonaws.com port=5432 user=user dbname=app sslmode = disable"
		pc := parse(t, dsn)
		require.NoError(t, configureTLS(pc, Config{URL: dsn}))
		assert.Nil(t, pc.ConnConfig.TLSConfig,
			"a spaced sslmode keyword is an explicit choice and must not be replaced by auto-TLS")
	})

	t.Run("uppercased RDS host without sslmode gets verify-full like its lowercase spelling", func(t *testing.T) {
		url := "postgres://user@MYDB.ABC123.US-EAST-1.RDS.AMAZONAWS.COM:5432/app"
		pc := parse(t, url)
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		require.NotNil(t, pc.ConnConfig.TLSConfig)
		assert.NotNil(t, pc.ConnConfig.TLSConfig.RootCAs)
		assert.False(t, pc.ConnConfig.TLSConfig.InsecureSkipVerify)
		assert.Nil(t, pc.ConnConfig.Fallbacks, "plaintext fallbacks must be dropped for RDS hosts")
	})

	t.Run("GovCloud RDS host is left untouched because the embedded bundle has no GovCloud roots", func(t *testing.T) {
		url := "postgres://user@mydb.abc123.us-gov-west-1.rds.amazonaws.com:5432/app"
		pc := parse(t, url)
		before := pc.ConnConfig.TLSConfig
		beforeFallbacks := pc.ConnConfig.Fallbacks
		require.NoError(t, configureTLS(pc, Config{URL: url}))
		assert.Equal(t, before, pc.ConnConfig.TLSConfig)
		assert.Equal(t, beforeFallbacks, pc.ConnConfig.Fallbacks)
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
