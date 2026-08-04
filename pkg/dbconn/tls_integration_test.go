package dbconn_test

import (
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
