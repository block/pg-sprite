package dbconn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultTableLockKeepalive = 10 * time.Second

// ErrInvariantViolation is returned when a dbconn proof cannot be constructed safely.
var ErrInvariantViolation = errors.New("invariant violation")

// TableLockHeldError reports that another session holds a table's advisory lock.
type TableLockHeldError struct {
	Schema string
	Table  string
}

func (e *TableLockHeldError) Error() string {
	return fmt.Sprintf("table lock is already held for %s.%s", e.Schema, e.Table)
}

// TableLock proves this process holds LK-1's per-table advisory lock on a
// dedicated session. Its zero value is forgeable; consumers must reject it
// when Table is empty.
type TableLock struct {
	schema, table string
	key           int64
}

// Schema returns the locked table's schema.
func (l TableLock) Schema() string { return l.schema }

// Table returns the locked table name.
func (l TableLock) Table() string { return l.table }

// Key returns the advisory lock key.
func (l TableLock) Key() int64 { return l.key }

// TableLockOption configures lock-session monitoring.
type TableLockOption func(*tableLockOptions) error

type tableLockOptions struct {
	keepalive time.Duration
}

// WithTableLockKeepalive sets the interval between session liveness checks.
func WithTableLockKeepalive(interval time.Duration) TableLockOption {
	return func(opts *tableLockOptions) error {
		if interval <= 0 {
			return fmt.Errorf("%w: LK-1: keepalive interval must be positive", ErrInvariantViolation)
		}
		opts.keepalive = interval
		return nil
	}
}

// TableLockSession owns the dedicated PostgreSQL session carrying a TableLock.
type TableLockSession struct {
	lock TableLock
	pid  uint32
	conn *pgx.Conn

	stop      chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	opMu      sync.Mutex
	releaseMu sync.Mutex

	stateMu    sync.Mutex
	lostErr    error
	released   bool
	releaseErr error
}

// AcquireTableLock opens a dedicated, non-recycling connection and tries to
// acquire the per-table advisory lock. It never retries a held lock. ctx bounds
// acquisition only; Release or detected lock loss ends the acquired session.
// schema and table must be catalog names as stored, including case, rather than
// unquoted SQL identifiers that PostgreSQL has not yet folded.
func AcquireTableLock(ctx context.Context, cfg Config, schema, table string, options ...TableLockOption) (*TableLockSession, error) {
	if schema == "" || table == "" {
		return nil, fmt.Errorf("%w: LK-1: table lock requires non-empty schema and table", ErrInvariantViolation)
	}
	opts := tableLockOptions{keepalive: defaultTableLockKeepalive}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: LK-1: nil table lock option", ErrInvariantViolation)
		}
		if err := option(&opts); err != nil {
			return nil, err
		}
	}

	pc, err := buildPoolConfig(cfg)
	if err != nil {
		return nil, err
	}
	connConfig := pc.ConnConfig.Copy()
	if cfg.BeforeConnect != nil {
		if err := cfg.BeforeConnect(ctx, connConfig); err != nil {
			return nil, fmt.Errorf("before connect for table lock: %w", err)
		}
	}
	// INV: LK-1 — a bare dedicated connection has no pool lifetime or idle
	// recycler that can silently replace the session carrying the lock.
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return nil, fmt.Errorf("connect dedicated table lock session: %w", err)
	}
	// The dedicated session is prepared exactly like a pooled one — with
	// session bounds and a catalog-first search_path.
	if err := pc.AfterConnect(ctx, conn); err != nil {
		return nil, errors.Join(fmt.Errorf("prepare dedicated table lock session: %w", err), conn.Close(ctx))
	}

	qualified := tableLockQualifiedName(schema, table)
	var key int64
	var held bool
	var pid uint32
	// INV: LK-1 — the server derives and attempts the key atomically on the
	// dedicated session, so no unprotected window follows a client-side hash.
	err = conn.QueryRow(ctx, `SELECT hashtext($1)::bigint,
		pg_try_advisory_lock(hashtext($1)::bigint), pg_backend_pid()`, qualified).Scan(&key, &held, &pid)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("acquire table lock for %s: %w", qualified, err), conn.Close(ctx))
	}
	if !held {
		return nil, errors.Join(&TableLockHeldError{Schema: schema, Table: table}, conn.Close(ctx))
	}
	// INV: LK-1 — acquisition is not a grant until this same connection can
	// observe the session-scoped lock it reported taking.
	confirmed, err := sessionHoldsAdvisoryLock(ctx, conn, key)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("confirm acquired table lock for %s: %w", qualified, err), conn.Close(ctx))
	}
	if !confirmed {
		return nil, errors.Join(fmt.Errorf("%w: LK-1: acquired table lock for %s is not held by its session", ErrInvariantViolation, qualified), conn.Close(ctx))
	}

	s := &TableLockSession{
		lock: TableLock{schema: schema, table: table, key: key}, pid: pid, conn: conn,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	s.wg.Go(func() { s.monitor(context.WithoutCancel(ctx), opts) })
	return s, nil
}

func tableLockQualifiedName(schema, table string) string { return schema + "." + table }

func (s *TableLockSession) monitor(ctx context.Context, opts tableLockOptions) {
	ticker := time.NewTicker(opts.keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			checkBound := max(opts.keepalive, DefaultConnectTimeout)
			checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkBound)
			s.opMu.Lock()
			held, err := sessionHoldsAdvisoryLock(checkCtx, s.conn, s.lock.key)
			s.opMu.Unlock()
			cancel()
			if err != nil {
				// INV: LK-1 — an unconfirmable dedicated session is lock loss.
				s.markLost(fmt.Errorf("table lock keepalive: %w", err))
				s.closeAfterLoss(ctx)
				return
			}
			if !held {
				// INV: LK-1 — a responsive session without its lock is lock loss.
				s.markLost(fmt.Errorf("%w: LK-1: table lock is no longer held by its session", ErrInvariantViolation))
				s.closeAfterLoss(ctx)
				return
			}
		}
	}
}

func (s *TableLockSession) closeAfterLoss(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), DefaultConnectTimeout)
	defer cancel()
	s.opMu.Lock()
	err := s.conn.Close(ctx)
	s.opMu.Unlock()
	if err != nil {
		s.stateMu.Lock()
		s.lostErr = errors.Join(s.lostErr, fmt.Errorf("close lost table lock session: %w", err))
		s.stateMu.Unlock()
	}
}

func (s *TableLockSession) markLost(err error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.lostErr != nil || s.released {
		return
	}
	s.lostErr = err
	close(s.done)
}

// Lock returns the proof carried by this session.
func (s *TableLockSession) Lock() TableLock { return s.lock }

// BackendPID returns the dedicated session's backend PID.
func (s *TableLockSession) BackendPID() uint32 { return s.pid }

// Done closes when the session is released or loses the lock unexpectedly.
func (s *TableLockSession) Done() <-chan struct{} { return s.done }

// Err returns nil while held or after a clean release, and the reason after loss.
func (s *TableLockSession) Err() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.lostErr
}

// Release unlocks and closes the dedicated session. Calls after the first
// return the first call's result.
func (s *TableLockSession) Release(ctx context.Context) error {
	s.releaseMu.Lock()
	defer s.releaseMu.Unlock()
	s.stateMu.Lock()
	if s.released {
		err := s.releaseErr
		s.stateMu.Unlock()
		return err
	}
	s.released = true
	close(s.stop)
	if s.lostErr == nil {
		close(s.done)
	}
	s.stateMu.Unlock()
	s.wg.Wait()

	s.opMu.Lock()
	var unlocked bool
	err := s.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", s.lock.key).Scan(&unlocked)
	if err == nil && !unlocked {
		err = fmt.Errorf("%w: LK-1: advisory lock was not held during release", ErrInvariantViolation)
	}
	closeErr := s.conn.Close(ctx)
	s.opMu.Unlock()
	result := errors.Join(err, closeErr)
	s.stateMu.Lock()
	s.releaseErr = result
	s.stateMu.Unlock()
	return result
}
