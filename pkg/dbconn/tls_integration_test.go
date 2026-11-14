package dbconn_test

import (
	"net"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// TestTLSIntegration proves the verify-full CACertPath path against a
// server that only accepts TLS connections: the handshake succeeds with the
// trusted CA, actually encrypts the session, and fails closed both without
// TLS and with a CA that did not sign the server certificate.
func TestTLSIntegration(t *testing.T) {
	pg := testutil.StartPostgresTLS(t)

	t.Run("server refuses non-TLS connections", func(t *testing.T) {
		_, err := dbconn.NewPool(t.Context(), dbconn.Config{
			URL: withParam(pg.URL, "sslmode=disable"),
		})
		require.Error(t, err, "hostssl-only server must reject a plaintext connection")
	})

	t.Run("verify-full with trusted CA succeeds and encrypts", func(t *testing.T) {
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
			URL:        pg.URL,
			CACertPath: pg.CACertPath,
		})
		require.NoError(t, err)
		t.Cleanup(pool.Close)

		var sslInUse bool
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&sslInUse))
		assert.True(t, sslInUse, "session must actually be TLS-encrypted")
	})

	t.Run("verification fails with untrusted CA", func(t *testing.T) {
		_, err := dbconn.NewPool(t.Context(), dbconn.Config{
			URL:        pg.URL,
			CACertPath: pg.UntrustedCACertPath,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certificate",
			"failure must be a certificate verification error, got: %v", err)
	})
}

// TestTLSIntegrationNoPlaintextFallback proves CACertPath never degrades to
// an unencrypted or unverified session. The server accepts both TLS and
// plaintext, so a client that fell back after a failed verification would
// connect — and the pool must refuse instead.
func TestTLSIntegrationNoPlaintextFallback(t *testing.T) {
	pg := testutil.StartPostgresTLSOrPlaintext(t)

	t.Run("fixture accepts plaintext", func(t *testing.T) {
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
			URL: withParam(pg.URL, "sslmode=disable"),
		})
		require.NoError(t, err, "without it the refusals below would prove nothing")
		t.Cleanup(pool.Close)
	})

	for name, param := range map[string]string{
		"no sslmode, which pgx treats as prefer": "",
		"sslmode=prefer":                         "sslmode=prefer",
		"sslmode=allow":                          "sslmode=allow",
	} {
		t.Run("untrusted CA is refused with "+name, func(t *testing.T) {
			url := pg.URL
			if param != "" {
				url = withParam(url, param)
			}
			_, err := dbconn.NewPool(t.Context(), dbconn.Config{
				URL:        url,
				CACertPath: pg.UntrustedCACertPath,
			})
			require.Error(t, err, "a failed verification must not fall back to a plaintext or unverified session")
			assert.Contains(t, err.Error(), "certificate",
				"failure must be a certificate verification error, got: %v", err)
		})
	}

	t.Run("trusted CA still connects over TLS", func(t *testing.T) {
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
			URL:        pg.URL,
			CACertPath: pg.CACertPath,
		})
		require.NoError(t, err)
		t.Cleanup(pool.Close)

		var sslInUse bool
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&sslInUse))
		assert.True(t, sslInUse, "session must actually be TLS-encrypted")
	})
}

// TestTLSIntegrationFailsOverToAVerifiedHost proves the per-host attempts
// pinEveryAttempt builds survive pgx's real dial and handshake: with the
// first host unreachable, the pool fails over to the second host and
// verifies it, and a second host whose certificate the CA did not sign is
// refused rather than reached over plaintext. The server accepts plaintext
// too, so a fallback that skipped verification would connect.
func TestTLSIntegrationFailsOverToAVerifiedHost(t *testing.T) {
	pg := testutil.StartPostgresTLSOrPlaintext(t)
	url := withUnreachableFirstHost(t, pg.URL)

	t.Run("trusted CA connects to the second host over TLS", func(t *testing.T) {
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
			URL:        url,
			CACertPath: pg.CACertPath,
		})
		require.NoError(t, err, "the pool must fail over past the unreachable host")
		t.Cleanup(pool.Close)

		var sslInUse bool
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&sslInUse))
		assert.True(t, sslInUse, "the failed-over session must be TLS-encrypted")
	})

	t.Run("untrusted CA is refused on the second host", func(t *testing.T) {
		_, err := dbconn.NewPool(t.Context(), dbconn.Config{
			URL:        url,
			CACertPath: pg.UntrustedCACertPath,
		})
		require.Error(t, err, "failover must not reach the second host unverified or over plaintext")
		assert.Contains(t, err.Error(), "certificate",
			"failure must be a certificate verification error, got: %v", err)
	})
}

// withUnreachableFirstHost puts a closed local port in front of the host of
// a connection URL, so pgx's first attempt is refused and it fails over.
func withUnreachableFirstHost(t *testing.T, raw string) string {
	t.Helper()
	l, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := l.Addr().String()
	require.NoError(t, l.Close())

	u, err := neturl.Parse(raw)
	require.NoError(t, err)
	u.Host = closed + "," + u.Host
	return u.String()
}

// withParam appends a query parameter to a connection URL regardless of
// whether it already carries parameters.
func withParam(url, param string) string {
	switch {
	case strings.HasSuffix(url, "?"), strings.HasSuffix(url, "&"):
		return url + param
	case strings.Contains(url, "?"):
		return url + "&" + param
	default:
		return url + "?" + param
	}
}
