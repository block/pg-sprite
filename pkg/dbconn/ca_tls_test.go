package dbconn

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigureTLSCACertPinsEveryAttempt proves that with a CA bundle set,
// every dial attempt pgx will make — the primary and each fallback — carries
// a verifying TLS config for its own host. pgx builds an unencrypted
// fallback for the default sslmode (prefer) and an unverified-TLS fallback
// for sslmode=allow; either would let a server that fails verification pull
// the pool onto an unauthenticated session.
func TestConfigureTLSCACertPinsEveryAttempt(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, rdsGlobalBundle, 0o600))

	tests := []struct {
		name      string
		url       string
		wantHosts []string
	}{
		{
			name:      "no sslmode, which pgx treats as prefer with a plaintext fallback",
			url:       "postgres://user@db1.example:5432/app",
			wantHosts: []string{"db1.example"},
		},
		{
			name:      "sslmode=prefer",
			url:       "postgres://user@db1.example:5432/app?sslmode=prefer",
			wantHosts: []string{"db1.example"},
		},
		{
			name:      "sslmode=allow, whose first attempt is plaintext",
			url:       "postgres://user@db1.example:5432/app?sslmode=allow",
			wantHosts: []string{"db1.example"},
		},
		{
			name:      "sslmode=require",
			url:       "postgres://user@db1.example:5432/app?sslmode=require",
			wantHosts: []string{"db1.example"},
		},
		{
			name:      "sslmode=verify-ca, which skips host name verification",
			url:       "postgres://user@db1.example:5432/app?sslmode=verify-ca",
			wantHosts: []string{"db1.example"},
		},
		{
			name:      "multi-host keeps one verified attempt per host",
			url:       "host=db1.example,db2.example port=5432,5433 user=user dbname=app",
			wantHosts: []string{"db1.example", "db2.example"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc, err := pgxpool.ParseConfig(tt.url)
			require.NoError(t, err)

			require.NoError(t, configureTLS(pc, Config{URL: tt.url, CACertPath: caPath}))

			cc := pc.ConnConfig
			gotHosts := []string{cc.Host}
			gotTLS := []*tls.Config{cc.TLSConfig}
			for _, fb := range cc.Fallbacks {
				gotHosts = append(gotHosts, fb.Host)
				gotTLS = append(gotTLS, fb.TLSConfig)
			}
			require.Equal(t, tt.wantHosts, gotHosts)
			for i, host := range gotHosts {
				require.NotNil(t, gotTLS[i], "attempt for %s must not be plaintext", host)
				assert.Equal(t, host, gotTLS[i].ServerName)
				assert.NotNil(t, gotTLS[i].RootCAs, "attempt for %s must verify against the bundle", host)
				assert.False(t, gotTLS[i].InsecureSkipVerify, "attempt for %s must verify the certificate", host)
				assert.Nil(t, gotTLS[i].VerifyPeerCertificate, "attempt for %s must not swap in a custom verifier", host)
			}
		})
	}
}

// TestConfigureTLSCACertRefusesSSLModeDisable proves a CA bundle together
// with an explicit sslmode=disable fails closed instead of silently picking
// one of the two contradictory requests.
func TestConfigureTLSCACertRefusesSSLModeDisable(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, rdsGlobalBundle, 0o600))

	for name, dsn := range map[string]string{
		"URL form":     "postgres://user@db1.example:5432/app?sslmode=disable",
		"keyword form": "host=db1.example user=user dbname=app sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			pc, err := pgxpool.ParseConfig(dsn)
			require.NoError(t, err)
			require.ErrorIs(t, configureTLS(pc, Config{URL: dsn, CACertPath: caPath}), ErrCACertWithSSLModeDisable)
		})
	}
}
