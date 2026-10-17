package dbconn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			name:      "sslmode=verify-ca is promoted to verify-full, host name included",
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

// TestConfigureTLSCACertKeepsClientSettings proves the bundle replaces only
// how the server is verified: each attempt, the multi-host fallback
// included, still presents the client certificate from sslcert / sslkey and
// offers the ALPN protocol sslnegotiation=direct needs.
func TestConfigureTLSCACertKeepsClientSettings(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, rdsGlobalBundle, 0o600))
	certPath, keyPath := writeClientCert(t, dir)

	dsn := "host=db1.example,db2.example user=user dbname=app sslmode=verify-full sslnegotiation=direct sslcert=" + certPath + " sslkey=" + keyPath
	pc, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	require.NoError(t, configureTLS(pc, Config{URL: dsn, CACertPath: caPath}))

	cc := pc.ConnConfig
	require.Len(t, cc.Fallbacks, 1)
	for _, c := range []*tls.Config{cc.TLSConfig, cc.Fallbacks[0].TLSConfig} {
		require.NotNil(t, c)
		assert.Len(t, c.Certificates, 1, "attempt for %s presents the client certificate", c.ServerName)
		assert.Equal(t, []string{"postgresql"}, c.NextProtos, "attempt for %s offers the direct-negotiation protocol", c.ServerName)
		assert.NotNil(t, c.RootCAs, "attempt for %s still verifies against the bundle", c.ServerName)
	}
}

// TestConfigureTLSCACertKeepsEachPortOfOneHost proves attempts are distinct
// by host and port: one host with two listeners (a server and a pooler)
// keeps both in the failover list, each verified against the host name.
func TestConfigureTLSCACertKeepsEachPortOfOneHost(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, rdsGlobalBundle, 0o600))
	dsn := "host=db1.example,db1.example port=5432,6432 user=user dbname=app"
	pc, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	require.NoError(t, configureTLS(pc, Config{URL: dsn, CACertPath: caPath}))

	cc := pc.ConnConfig
	require.Len(t, cc.Fallbacks, 1)
	assert.Equal(t, uint16(5432), cc.Port)
	assert.Equal(t, uint16(6432), cc.Fallbacks[0].Port)
	require.NotNil(t, cc.Fallbacks[0].TLSConfig)
	assert.Equal(t, "db1.example", cc.Fallbacks[0].TLSConfig.ServerName)
}

// TestConfigureTLSCACertLeavesUnixSocketsLocal proves a unix-domain socket
// in the host list keeps pgx's nil TLS config — PostgreSQL does not speak
// TLS over a socket — while the network host beside it is verified.
func TestConfigureTLSCACertLeavesUnixSocketsLocal(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, rdsGlobalBundle, 0o600))
	dsn := "host=db1.example,/var/run/postgresql user=user dbname=app sslmode=require"
	pc, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	require.NoError(t, configureTLS(pc, Config{URL: dsn, CACertPath: caPath}))

	cc := pc.ConnConfig
	require.NotNil(t, cc.TLSConfig)
	assert.Equal(t, "db1.example", cc.TLSConfig.ServerName)
	require.Len(t, cc.Fallbacks, 1)
	assert.Equal(t, "/var/run/postgresql", cc.Fallbacks[0].Host)
	assert.Nil(t, cc.Fallbacks[0].TLSConfig, "a socket attempt carries no TLS")
}

// writeClientCert writes a self-signed client certificate and its key under
// dir and returns their paths.
func writeClientCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPath, keyPath = filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}
