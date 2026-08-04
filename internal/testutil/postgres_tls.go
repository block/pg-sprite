package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TLSPostgres describes a TLS-only PostgreSQL started by StartPostgresTLS.
type TLSPostgres struct {
	// URL is the connection URL without an sslmode parameter, so the
	// caller's TLS configuration decides the handshake.
	URL string
	// CACertPath is the PEM CA certificate that signed the server
	// certificate — the trust anchor for verify-full connections.
	CACertPath string
	// UntrustedCACertPath is a valid CA certificate that did NOT sign the
	// server certificate, for negative verification tests.
	UntrustedCACertPath string
}

// tlsInitScript runs as the postgres user during initdb: it installs the
// server certificate and restricts pg_hba to TLS-only TCP connections, so
// every network connection in the test must complete a TLS handshake.
const tlsInitScript = `#!/bin/sh
set -e
cp /tls/server.crt /tls/server.key "$PGDATA"/
chmod 0600 "$PGDATA"/server.key
cat >> "$PGDATA"/postgresql.conf <<EOF
ssl = on
ssl_cert_file = 'server.crt'
ssl_key_file = 'server.key'
EOF
cat > "$PGDATA"/pg_hba.conf <<EOF
local all all trust
hostssl all all 0.0.0.0/0 scram-sha-256
hostssl all all ::/0 scram-sha-256
EOF
`

// StartPostgresTLS starts a disposable PostgreSQL container that accepts
// only TLS connections, using a CA generated for this test. Unlike
// StartPostgres it never uses PG_DSN — the whole point is controlling the
// server's TLS posture. Set SKIP_INTEGRATION=1 to skip.
func StartPostgresTLS(t *testing.T) TLSPostgres {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs a database")
	}

	dir := t.TempDir()
	caPath, serverCrtPath, serverKeyPath := generateServerCertificates(t, dir)
	untrustedCAPath := filepath.Join(dir, "untrusted-ca.crt")
	writeCACert(t, untrustedCAPath)
	scriptPath := filepath.Join(dir, "000-tls.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(tlsInitScript), 0o755))

	// t.Context only governs the start request; the running container is
	// not tied to it and is terminated via t.Cleanup below.
	ctx := t.Context()
	ctr, err := tcpostgres.Run(ctx, "postgres:"+PGVersion(),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Files: []testcontainers.ContainerFile{
					{HostFilePath: serverCrtPath, ContainerFilePath: "/tls/server.crt", FileMode: 0o644},
					{HostFilePath: serverKeyPath, ContainerFilePath: "/tls/server.key", FileMode: 0o644},
					{HostFilePath: scriptPath, ContainerFilePath: "/docker-entrypoint-initdb.d/000-tls.sh", FileMode: 0o755},
				},
			},
		}),
	)
	require.NoError(t, err, "start TLS postgres container")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate TLS postgres container: %v", err)
		}
	})

	url, err := ctr.ConnectionString(ctx)
	require.NoError(t, err, "container connection string")
	return TLSPostgres{
		URL:                 url,
		CACertPath:          caPath,
		UntrustedCACertPath: untrustedCAPath,
	}
}

// generateServerCertificates creates a throwaway CA and a server certificate
// for localhost signed by it, writes all three PEM files into dir, and
// returns their paths (CA cert, server cert, server key).
func generateServerCertificates(t *testing.T, dir string) (caPath, crtPath, keyPath string) {
	t.Helper()

	caCert, caKey := newCA(t)
	caPath = filepath.Join(dir, "ca.crt")
	writePEM(t, caPath, "CERTIFICATE", caCert.Raw)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(t),
		Subject:      pkix.Name{CommonName: "pg-sprite test server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	crtPath = filepath.Join(dir, "server.crt")
	writePEM(t, crtPath, "CERTIFICATE", der)

	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	require.NoError(t, err)
	keyPath = filepath.Join(dir, "server.key")
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
	return caPath, crtPath, keyPath
}

// writeCACert generates an independent CA and writes its certificate to path.
func writeCACert(t *testing.T, path string) {
	t.Helper()
	caCert, _ := newCA(t)
	writePEM(t, path, "CERTIFICATE", caCert.Raw)
}

// newCA generates a self-signed CA certificate and its key.
func newCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          newSerial(t),
		Subject:               pkix.Name{CommonName: "pg-sprite test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

func newSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	return serial
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o644))
}
