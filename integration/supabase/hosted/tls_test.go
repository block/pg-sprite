package hosted_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TLS checks need only a database credential; they create no tables or Auth users.
func hostedTLSURL(t *testing.T, mode string, keepCA bool) string {
	t.Helper()
	if os.Getenv("SUPABASE_HOSTED_TEST") != "1" {
		t.Skip("set SUPABASE_HOSTED_TEST=1 for hosted checks")
	}
	u, err := url.Parse(os.Getenv("PGSPRITE_URL"))
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, u.Scheme)
	require.True(t, strings.HasPrefix(u.Hostname(), "db.") && strings.HasSuffix(u.Hostname(), ".supabase.co"), "use a hosted direct endpoint")
	q := u.Query()
	if mode == "" {
		q.Del("sslmode")
	} else {
		q.Set("sslmode", mode)
	}
	if !keepCA {
		q.Del("sslrootcert")
	} else if q.Get("sslrootcert") == "" {
		t.Skip("set sslrootcert to the downloaded Supabase CA for verification tests")
	}
	u.RawQuery = q.Encode()
	return u.String()
}
func assertHostedTLS(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	pool, err := dbconn.NewPool(ctx, dbconn.Config{URL: dsn})
	require.NoError(t, err)
	defer pool.Close()
	var ssl bool
	var version string
	require.NoError(t, pool.QueryRow(ctx, "SELECT ssl,version FROM pg_stat_ssl WHERE pid=pg_backend_pid()").Scan(&ssl, &version))
	assert.True(t, ssl)
	t.Logf("encrypted session: %s", version)
}
func TestHostedTLSDefault(t *testing.T) {
	assertHostedTLS(t, hostedTLSURL(t, "", false))
}
func TestHostedTLSRequire(t *testing.T) {
	assertHostedTLS(t, hostedTLSURL(t, "require", false))
}
func TestHostedTLSVerifyFull(t *testing.T) {
	assertHostedTLS(t, hostedTLSURL(t, "verify-full", true))
}
func TestHostedTLSRejectUntrustedCA(t *testing.T) {
	dsn := hostedTLSURL(t, "verify-full", true)
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("sslrootcert", untrustedCA(t))
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	pool, err := dbconn.NewPool(ctx, dbconn.Config{URL: u.String()})
	if pool != nil {
		defer pool.Close()
	}
	var untrusted x509.UnknownAuthorityError
	require.ErrorAs(t, err, &untrusted, "network/auth errors are not proof of certificate rejection")
}
func TestHostedTLSRejectWrongHostname(t *testing.T) {
	dsn := hostedTLSURL(t, "verify-full", true)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	require.NotNil(t, cfg.ConnConfig.TLSConfig)
	require.False(t, cfg.ConnConfig.TLSConfig.InsecureSkipVerify)
	require.Empty(t, cfg.ConnConfig.Fallbacks)
	// Keep the real dial address and CA, changing only the expected identity.
	cfg.ConnConfig.TLSConfig.ServerName = "wrong-host.invalid"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()
	err = pool.Ping(ctx)
	var mismatch x509.HostnameError
	require.ErrorAs(t, err, &mismatch)
}
func untrustedCA(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Untrusted test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "untrusted.crt")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	return path
}
