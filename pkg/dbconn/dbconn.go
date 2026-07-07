// Package dbconn is the engine's database connectivity layer: pgx pool
// construction with safe session defaults (lock_timeout, statement_timeout),
// optional RDS/Aurora CA TLS, bounded retries for transient errors, and a
// helper to terminate backends blocking a session's lock acquisition.
package dbconn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Defaults for the session timeouts every pooled connection runs under. Every
// statement the engine issues is bounded: lock_timeout keeps the engine from
// sitting at the head of the lock queue (the lock-queue pile-up), and
// statement_timeout bounds runaway work.
const (
	DefaultLockTimeout      = 3 * time.Second
	DefaultStatementTimeout = 30 * time.Second
)

// Config describes a connection target.
type Config struct {
	// URL is a libpq connection string or URL (postgres://...).
	URL string
	// LockTimeout is applied as the session lock_timeout on every connection.
	// Zero means DefaultLockTimeout.
	LockTimeout time.Duration
	// StatementTimeout is applied as the session statement_timeout on every
	// connection. Zero means DefaultStatementTimeout.
	StatementTimeout time.Duration
	// CACertPath, when set, enables verify-full TLS using the given CA bundle
	// (e.g. the RDS/Aurora global bundle).
	CACertPath string
	// MaxConns caps the pool size. Zero keeps the pgxpool default.
	MaxConns int32
}

// NewPool builds a pgx pool from cfg, applies the session defaults, and
// verifies connectivity with a ping before returning.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse connection config: %w", err)
	}

	lockTimeout := cfg.LockTimeout
	if lockTimeout == 0 {
		lockTimeout = DefaultLockTimeout
	}
	stmtTimeout := cfg.StatementTimeout
	if stmtTimeout == 0 {
		stmtTimeout = DefaultStatementTimeout
	}
	rp := pc.ConnConfig.RuntimeParams
	// A bare integer is interpreted by PostgreSQL as milliseconds.
	rp["lock_timeout"] = strconv.FormatInt(lockTimeout.Milliseconds(), 10)
	rp["statement_timeout"] = strconv.FormatInt(stmtTimeout.Milliseconds(), 10)
	rp["application_name"] = "pg-sprite"

	if err := configureTLS(pc, cfg); err != nil {
		return nil, err
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping after connect: %w", err)
	}
	return pool, nil
}

// configureTLS decides the pool's TLS setup: an explicit CA bundle wins;
// otherwise RDS/Aurora endpoints get TLS automatically via the embedded RDS
// global bundle (see rds.go); everything else keeps whatever the connection
// string asked for.
func configureTLS(pc *pgxpool.Config, cfg Config) error {
	switch {
	case cfg.CACertPath != "":
		tlsCfg, err := caTLSConfig(cfg.CACertPath, pc.ConnConfig.Host)
		if err != nil {
			return err
		}
		pc.ConnConfig.TLSConfig = tlsCfg
	case IsRDSHost(pc.ConnConfig.Host):
		if strings.Contains(cfg.URL, "sslmode=") {
			// The caller chose an sslmode; honor it — but when verification
			// was requested without a root bundle, supply the embedded RDS
			// roots, which are not in system trust stores.
			if tc := pc.ConnConfig.TLSConfig; tc != nil && !tc.InsecureSkipVerify && tc.RootCAs == nil {
				tc.RootCAs = rdsRootPool()
			}
		} else {
			// No explicit sslmode on an RDS/Aurora endpoint: auto-enable
			// verify-full with the embedded bundle, and drop the plaintext
			// fallbacks the default sslmode would otherwise allow.
			pc.ConnConfig.TLSConfig = rdsTLSConfig(pc.ConnConfig.Host)
			pc.ConnConfig.Fallbacks = nil
		}
	}
	return nil
}

// caTLSConfig builds a verify-full TLS config trusting only the given CA
// bundle, verifying the server certificate against host.
func caTLSConfig(caCertPath, host string) (*tls.Config, error) {
	pem, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %s contains no usable certificates", caCertPath)
	}
	return &tls.Config{
		RootCAs:    roots,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}, nil
}
