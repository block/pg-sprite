package dbconn

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectSession opens one dedicated connection on cfg's database, outside
// any pool, prepared exactly as a pooled connection would be: the same TLS
// posture, connect timeout, startup parameters, and BeforeConnect hook, and
// the same session bounds (lock_timeout and statement_timeout, LK-2) and
// catalog-first search_path applied once connected. It is for work that
// must not compete with a pool's connections for a seat — a lock held open
// while the holder draws on the pool — and that is not exclusive to one
// server session the way a session-level lock is; that one is
// AcquireTableLock, which also proves the session's affinity. The caller
// closes the connection; nothing recycles it.
func ConnectSession(ctx context.Context, cfg Config) (*pgx.Conn, error) {
	conn, _, err := connectDedicated(ctx, cfg)
	return conn, err
}

// connectDedicated dials one bare connection from cfg and runs the pool's
// AfterConnect hooks on it, returning the pool configuration it was built
// from for a caller with more to prove about the session.
func connectDedicated(ctx context.Context, cfg Config) (*pgx.Conn, *pgxpool.Config, error) {
	pc, err := buildPoolConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	connConfig := pc.ConnConfig.Copy()
	if cfg.BeforeConnect != nil {
		if err := cfg.BeforeConnect(ctx, connConfig); err != nil {
			return nil, nil, fmt.Errorf("before connect for dedicated session: %w", err)
		}
	}
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("connect dedicated session: %w", err)
	}
	if err := pc.AfterConnect(ctx, conn); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("prepare dedicated session: %w", err), conn.Close(ctx))
	}
	return conn, pc, nil
}
