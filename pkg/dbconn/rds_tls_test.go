package dbconn

import (
	"crypto/tls"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rdsWriterHost  = "writer.abc123.us-east-1.rds.amazonaws.com"
	rdsReplicaHost = "replica.abc123.us-west-2.rds.amazonaws.com"
	govCloudHost   = "db.abc123.us-gov-west-1.rds.amazonaws.com"
)

// parseForTLS parses dsn the way buildPoolConfig does and runs configureTLS
// on it without a CA bundle.
func parseForTLS(t *testing.T, dsn string) (*pgxpool.Config, error) {
	t.Helper()
	pc, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	return pc, configureTLS(pc, Config{URL: dsn})
}

// TestConfigureTLSRDSAutoTLSKeepsEveryHost proves a multi-host URL of
// RDS/Aurora endpoints with no sslmode keeps its failover hosts, each with
// exactly one attempt that verifies its own host name against the embedded
// bundle; the plaintext attempts pgx's default sslmode adds are gone.
func TestConfigureTLSRDSAutoTLSKeepsEveryHost(t *testing.T) {
	pc, err := parseForTLS(t, "postgres://user@"+rdsWriterHost+":5432,"+rdsReplicaHost+":5433/app")
	require.NoError(t, err)

	cc := pc.ConnConfig
	require.Len(t, cc.Fallbacks, 1, "the replica stays in the failover list, once")
	assert.Equal(t, rdsWriterHost, cc.Host)
	assert.Equal(t, uint16(5432), cc.Port)
	assert.Equal(t, rdsReplicaHost, cc.Fallbacks[0].Host)
	assert.Equal(t, uint16(5433), cc.Fallbacks[0].Port)
	assertVerifiesAgainstRDSBundle(t, cc.TLSConfig, rdsWriterHost)
	assertVerifiesAgainstRDSBundle(t, cc.Fallbacks[0].TLSConfig, rdsReplicaHost)
	assert.Equal(t, uint16(tls.VersionTLS12), cc.Fallbacks[0].TLSConfig.MinVersion)
}

// TestConfigureTLSRDSAutoTLSKeepsClientSettings proves auto-TLS replaces
// only how the server is verified: every attempt still presents the client
// certificate from sslcert / sslkey and offers the ALPN protocol
// sslnegotiation=direct needs.
func TestConfigureTLSRDSAutoTLSKeepsClientSettings(t *testing.T) {
	certPath, keyPath := writeClientCert(t, t.TempDir())
	pc, err := parseForTLS(t, "host="+rdsWriterHost+","+rdsReplicaHost+" user=user dbname=app sslnegotiation=direct sslcert="+certPath+" sslkey="+keyPath)
	require.NoError(t, err)

	cc := pc.ConnConfig
	require.Len(t, cc.Fallbacks, 1)
	for _, c := range []*tls.Config{cc.TLSConfig, cc.Fallbacks[0].TLSConfig} {
		require.NotNil(t, c)
		assert.Len(t, c.Certificates, 1, "attempt for %s presents the client certificate", c.ServerName)
		assert.Equal(t, []string{"postgresql"}, c.NextProtos, "attempt for %s offers the direct-negotiation protocol", c.ServerName)
	}
}

// TestConfigureTLSRDSAutoTLSLeavesUnixSocketsLocal proves a unix-domain
// socket beside an RDS/Aurora endpoint keeps pgx's nil TLS config:
// PostgreSQL offers no TLS over a socket, and the socket never leaves the
// host.
func TestConfigureTLSRDSAutoTLSLeavesUnixSocketsLocal(t *testing.T) {
	pc, err := parseForTLS(t, "host="+rdsWriterHost+",/var/run/postgresql user=user dbname=app")
	require.NoError(t, err)

	cc := pc.ConnConfig
	assertVerifiesAgainstRDSBundle(t, cc.TLSConfig, rdsWriterHost)
	require.Len(t, cc.Fallbacks, 1)
	assert.Equal(t, "/var/run/postgresql", cc.Fallbacks[0].Host)
	assert.Nil(t, cc.Fallbacks[0].TLSConfig, "a socket attempt carries no TLS")
}

// TestConfigureTLSRDSAutoTLSRefusesMixedHosts proves auto-TLS fails closed
// when the URL lists an RDS/Aurora endpoint beside a host the embedded
// bundle cannot verify, whichever comes first: verifying every host would
// fail the other one's handshake, and leaving it on pgx's default would
// fall back to an unverified or plaintext session. The error names the
// host.
func TestConfigureTLSRDSAutoTLSRefusesMixedHosts(t *testing.T) {
	t.Run("RDS/Aurora primary with a self-managed fallback", func(t *testing.T) {
		_, err := parseForTLS(t, "host="+rdsWriterHost+",db2.example user=user dbname=app")
		require.ErrorIs(t, err, ErrMixedRDSAndOtherHosts)
		assert.ErrorContains(t, err, "db2.example")
	})
	t.Run("self-managed primary with an RDS/Aurora fallback", func(t *testing.T) {
		_, err := parseForTLS(t, "host=db1.example,"+rdsReplicaHost+" user=user dbname=app")
		require.ErrorIs(t, err, ErrMixedRDSAndOtherHosts)
		assert.ErrorContains(t, err, "db1.example")
	})
	t.Run("RDS/Aurora fallback in a partition the embedded bundle does not cover", func(t *testing.T) {
		_, err := parseForTLS(t, "host="+rdsWriterHost+","+govCloudHost+" user=user dbname=app")
		require.ErrorIs(t, err, ErrRDSBundleDoesNotCoverHost)
		assert.ErrorContains(t, err, govCloudHost)
	})
}

// TestConfigureTLSRDSVerifyFullSuppliesRootsToEveryAttempt proves an
// explicit sslmode=verify-full without sslrootcert gets the embedded RDS
// roots on every RDS/Aurora attempt, not just the primary: system trust
// stores do not carry them, so a failover host would fail its handshake.
func TestConfigureTLSRDSVerifyFullSuppliesRootsToEveryAttempt(t *testing.T) {
	pc, err := parseForTLS(t, "postgres://user@"+rdsWriterHost+":5432,"+rdsReplicaHost+":5433/app?sslmode=verify-full")
	require.NoError(t, err)

	cc := pc.ConnConfig
	require.Len(t, cc.Fallbacks, 1, "verify-full has one attempt per host")
	assertVerifiesAgainstRDSBundle(t, cc.TLSConfig, rdsWriterHost)
	assertVerifiesAgainstRDSBundle(t, cc.Fallbacks[0].TLSConfig, rdsReplicaHost)
}

// TestConfigureTLSRDSVerifyFullLeavesOtherHostsOnSystemRoots proves the
// explicit verify-full path supplies roots per host: a self-managed host
// beside an RDS/Aurora endpoint keeps verifying against the system roots,
// as the caller asked, while the endpoint gets the embedded bundle.
func TestConfigureTLSRDSVerifyFullLeavesOtherHostsOnSystemRoots(t *testing.T) {
	pc, err := parseForTLS(t, "host=db1.example,"+rdsReplicaHost+" user=user dbname=app sslmode=verify-full")
	require.NoError(t, err)

	cc := pc.ConnConfig
	require.NotNil(t, cc.TLSConfig)
	assert.Equal(t, "db1.example", cc.TLSConfig.ServerName)
	assert.Nil(t, cc.TLSConfig.RootCAs, "nil roots verify against the system trust store")
	require.Len(t, cc.Fallbacks, 1)
	assertVerifiesAgainstRDSBundle(t, cc.Fallbacks[0].TLSConfig, rdsReplicaHost)
}

// TestConfigureTLSRDSVerifyFullRefusesUncoveredFallback proves a fallback
// in a partition the embedded bundle does not cover is refused under
// verify-full without roots, naming that host, rather than left to fail
// its handshake with an x509 error that does not say why.
func TestConfigureTLSRDSVerifyFullRefusesUncoveredFallback(t *testing.T) {
	_, err := parseForTLS(t, "host="+rdsWriterHost+","+govCloudHost+" user=user dbname=app sslmode=verify-full")
	require.ErrorIs(t, err, ErrRDSBundleDoesNotCoverHost)
	assert.ErrorContains(t, err, govCloudHost)
}

// TestConfigureTLSRDSExplicitModeWithoutVerificationIsHonored proves an
// explicit sslmode that needs no roots is left as the caller chose on every
// attempt: prefer keeps its unverified and plaintext attempts per host.
func TestConfigureTLSRDSExplicitModeWithoutVerificationIsHonored(t *testing.T) {
	pc, err := parseForTLS(t, "host="+rdsWriterHost+","+govCloudHost+" user=user dbname=app sslmode=prefer")
	require.NoError(t, err)

	cc := pc.ConnConfig
	require.NotNil(t, cc.TLSConfig)
	assert.True(t, cc.TLSConfig.InsecureSkipVerify)
	assert.Nil(t, cc.TLSConfig.RootCAs)
	gotHosts := []string{}
	gotPlaintext := []bool{}
	for _, fb := range cc.Fallbacks {
		gotHosts = append(gotHosts, fb.Host)
		gotPlaintext = append(gotPlaintext, fb.TLSConfig == nil)
	}
	// pgx's prefer: encrypted-unverified, then plaintext, for each host.
	assert.Equal(t, []string{rdsWriterHost, govCloudHost, govCloudHost}, gotHosts)
	assert.Equal(t, []bool{true, false, true}, gotPlaintext)
}

// assertVerifiesAgainstRDSBundle checks c is a verify-full attempt for host
// against exactly the embedded RDS roots.
func assertVerifiesAgainstRDSBundle(t *testing.T, c *tls.Config, host string) {
	t.Helper()
	require.NotNil(t, c, "attempt for %s must not be plaintext", host)
	assert.Equal(t, host, c.ServerName)
	require.NotNil(t, c.RootCAs, "attempt for %s must carry roots", host)
	assert.True(t, c.RootCAs.Equal(rdsRootPool()), "attempt for %s must verify against the embedded RDS bundle", host)
	assert.False(t, c.InsecureSkipVerify, "attempt for %s must verify the certificate", host)
}
