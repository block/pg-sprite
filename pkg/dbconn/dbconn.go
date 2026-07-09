// Package dbconn is the engine's database connectivity layer: pgx pool
// construction with safe session defaults (lock_timeout, statement_timeout),
// RDS/Aurora TLS, bounded retries for transient errors, and a helper to
// terminate backends blocking a session's lock acquisition.
package dbconn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
)

// Defaults for the session timeouts every pooled connection runs under. Every
// statement the engine issues is bounded: lock_timeout keeps the engine from
// sitting at the head of the lock queue (the lock-queue pile-up), and
// statement_timeout bounds runaway work.
const (
	DefaultLockTimeout      = 3 * time.Second
	DefaultStatementTimeout = 30 * time.Second
	// DefaultConnectTimeout bounds each dial attempt.
	DefaultConnectTimeout = 10 * time.Second
)

// Config describes a connection target. Zero values keep sensible defaults
// (ours for the session timeouts, pgxpool's for pool sizing and lifecycle) —
// decisions, not options.
type Config struct {
	// URL is a libpq connection string or URL (postgres://...).
	URL string
	// LockTimeout is applied as the session lock_timeout on every connection.
	// Zero means DefaultLockTimeout.
	LockTimeout time.Duration
	// StatementTimeout is applied as the session statement_timeout on every
	// connection. Zero means DefaultStatementTimeout.
	StatementTimeout time.Duration
	// ConnectTimeout bounds each dial attempt. Zero means
	// DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// CACertPath, when set, enables verify-full TLS using the given CA bundle
	// (e.g. the RDS/Aurora global bundle). Unset, RDS/Aurora endpoints are
	// auto-verified with the embedded bundle (see rds.go).
	CACertPath string

	// Pool sizing and lifecycle. Zero values keep pgxpool's defaults.
	//
	// NOTE: the advisory-lock connection (LK-1) must NOT come from this pool:
	// session-scoped locks die with their session, and lifetime/idle
	// recycling would silently release the lock. The lock helper owns a
	// dedicated single-connection pool exempt from recycling.
	MaxConns              int32
	MinConns              int32
	MaxConnLifetime       time.Duration
	MaxConnLifetimeJitter time.Duration
	MaxConnIdleTime       time.Duration
	HealthCheckPeriod     time.Duration

	// QueryExecMode overrides pgx's default protocol usage — e.g.
	// pgx.QueryExecModeExec when a transaction-pooling proxy that cannot
	// handle prepared statements sits in front of the pool. Zero keeps pgx's
	// default (statement caching).
	QueryExecMode pgx.QueryExecMode
	// Logger, when set, enables statement-level tracing (pgx tracelog) at
	// debug level through the given slog logger.
	Logger *slog.Logger
	// BeforeConnect, when set, can mutate each new connection's config just
	// before dialing — the hook for short-lived credentials such as RDS IAM
	// authentication tokens.
	BeforeConnect func(context.Context, *pgx.ConnConfig) error
}

// NewPool builds a pgx pool from cfg, applies the session defaults, and
// verifies connectivity with a ping before returning.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pc, err := buildPoolConfig(cfg)
	if err != nil {
		return nil, err
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

// buildPoolConfig translates Config into a pgxpool configuration. It is pure
// (no dialing), so every option's wiring is unit-testable without a server.
func buildPoolConfig(cfg Config) (*pgxpool.Config, error) {
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

	connectTimeout := cfg.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = DefaultConnectTimeout
	}
	pc.ConnConfig.ConnectTimeout = connectTimeout

	if err := configureTLS(pc, cfg); err != nil {
		return nil, err
	}

	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnLifetimeJitter > 0 {
		pc.MaxConnLifetimeJitter = cfg.MaxConnLifetimeJitter
	}
	if cfg.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.HealthCheckPeriod > 0 {
		pc.HealthCheckPeriod = cfg.HealthCheckPeriod
	}
	if cfg.QueryExecMode != 0 {
		pc.ConnConfig.DefaultQueryExecMode = cfg.QueryExecMode
	}
	if cfg.BeforeConnect != nil {
		pc.BeforeConnect = cfg.BeforeConnect
	}
	if cfg.Logger != nil {
		pc.ConnConfig.Tracer = &tracelog.TraceLog{
			Logger:   slogTraceLogger{logger: cfg.Logger},
			LogLevel: tracelog.LogLevelDebug,
		}
	}
	return pc, nil
}

// slogTraceLogger adapts slog to pgx's tracelog logger interface.
type slogTraceLogger struct {
	logger *slog.Logger
}

func (l slogTraceLogger) Log(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) {
	attrs := make([]any, 0, len(data)*2)
	for k, v := range data {
		attrs = append(attrs, k, v)
	}
	var slogLevel slog.Level
	switch level {
	case tracelog.LogLevelTrace, tracelog.LogLevelDebug:
		slogLevel = slog.LevelDebug
	case tracelog.LogLevelInfo:
		slogLevel = slog.LevelInfo
	case tracelog.LogLevelWarn:
		slogLevel = slog.LevelWarn
	default:
		slogLevel = slog.LevelError
	}
	l.logger.Log(ctx, slogLevel, msg, attrs...)
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
