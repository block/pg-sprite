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
