package dbconn

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors a table lock acquisition distinguishes. They mean different things
// to a caller: a busy lock is another instance doing its job, a lost session
// binding is a deployment that cannot support the lock at all, and a lost
// lock is an in-flight change that must stop.
var (
	// ErrTableLocked reports that another instance holds the table's lock.
	// Nothing was executed, and the caller may retry later.
	ErrTableLocked = errors.New("another pg-sprite instance is changing this table")

	// ErrNoSessionAffinity reports that the connection does not keep one
	// server session, so a session-scoped advisory lock on it excludes
	// nobody. The usual cause is a transaction-mode connection pooler
	// (PgBouncer in transaction mode, Supavisor's pooled port, RDS Proxy
	// without pinning), which rebinds a backend per transaction: the lock
	// lands on a backend this client stops mapping to, a second instance
	// reaching that backend takes the same lock, and both are told they
	// hold it.
	ErrNoSessionAffinity = errors.New("the connection does not keep one PostgreSQL session, so an advisory lock on it excludes nobody")

	// ErrLockLost reports that the table lock was confirmed held at
	// acquisition and is not held now. Any change still running under it
	// has lost its exclusion and must stop.
	ErrLockLost = errors.New("the table lock is no longer held")
)

// Keepalive defaults. The interval is far shorter than any plausible
// server-side idle timeout, so an idle-session reaper cannot take the lock
// session between two confirmations, and the whole check is one indexed
// catalog read on a dedicated connection.
const (
	// DefaultKeepaliveInterval is how often the lock is re-confirmed held.
	DefaultKeepaliveInterval = 5 * time.Second
	// keepaliveTimeout bounds one confirmation. A confirmation that cannot
	// complete is a lost lock: the point of the check is that silence is
	// never read as health.
	keepaliveTimeout = 10 * time.Second
	// lockOperationTimeout bounds the acquisition's own statements.
	lockOperationTimeout = 15 * time.Second
)

// TableLockOptions tunes an acquisition. The zero value is the sanctioned
// policy: the default keepalive interval and no diagnostics.
type TableLockOptions struct {
	// KeepaliveInterval is how often the held lock is re-confirmed. Zero
	// means DefaultKeepaliveInterval.
	KeepaliveInterval time.Duration
	// Logger receives the lock's lifecycle diagnostics; nil discards them.
	Logger *slog.Logger
}

// TableLock is the proof that this process holds the exclusive right to
// change one table: a session-scoped advisory lock, confirmed held on a
// dedicated connection, on a connection proven to keep its server session.
// Only AcquireTableLock mints one, so a mutating operation that requires a
// TableLock cannot be called without the exclusion it needs.
//
// A held lock is not assumed to stay held. A keepalive re-confirms it on an
// interval, and the confirmation reads the catalog rather than the client's
// own memory of what it took, so a session killed by an idle reaper, a
// failover, or an operator's pg_terminate_backend is discovered rather than
// believed away. Discovery is fail-closed: the context every guarded
// operation runs under is cancelled, so an in-flight change stops instead of
// continuing unprotected.
type TableLock struct {
	schema string
	table  string
	key    int64
	pid    uint32

	pool *pgxpool.Pool
	// conn is held checked out for the lock's whole lifetime. A
	// session-scoped lock dies with its session, and a connection returned
	// to a pool can be recycled by lifetime or idle time, which would
	// release the lock silently. Holding it checked out is what makes the
	// pool's recycling policy unable to reach it.
	conn   *pgxpool.Conn
	connMu sync.Mutex

	logger *slog.Logger

	// lost is closed once the lock is known not to be held. cause records
	// why, read under lostMu.
	lost      chan struct{}
	lostOnce  sync.Once
	lostMu    sync.Mutex
	lostCause error

	// stop ends the keepalive; stopped closes when it has returned, so
	// Release never races the keepalive for the connection.
	stop     context.CancelFunc
	stopped  chan struct{}
	released sync.Once
}

// AcquireTableLock takes the table's advisory lock for this process, or
// reports why it could not.
//
// The lock is held on its own connection, dialed from pool's configuration
// and exempt from the pool's recycling, because a session-scoped lock dies
// with its session. Before taking it, the connection is proven to keep one
// server session (see ProveSessionAffinity): behind a transaction-mode
// pooler an advisory lock grants without excluding anyone, and a lock that
// silently stopped meaning anything is worse than no lock at all.
//
// A lock another instance holds returns ErrTableLocked and takes nothing.
// The caller closes the lock with Release, which is also what stops the
// keepalive.
//
// INV: LK-1 — at most one schema change runs per table.
func AcquireTableLock(ctx context.Context, pool *pgxpool.Pool, schema, table string, opts TableLockOptions) (*TableLock, error) {
	if schema == "" || table == "" {
		return nil, fmt.Errorf("table lock needs a schema-qualified table, got %q.%q", schema, table)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	interval := opts.KeepaliveInterval
	if interval == 0 {
		interval = DefaultKeepaliveInterval
	}

	lockPool, err := newLockPool(ctx, pool)
	if err != nil {
		return nil, err
	}
	conn, err := lockPool.Acquire(ctx)
	if err != nil {
		lockPool.Close()
		return nil, fmt.Errorf("acquire the table lock connection: %w", err)
	}
	// Until the lock is confirmed held, every failure path tears the whole
	// pool down rather than leaving a session whose advisory-lock state is
	// unknown: closing the pool ends the session, which drops anything it
	// still holds.
	held := false
	defer func() {
		if !held {
			closeLockPool(conn, lockPool)
		}
	}()

	database, err := currentDatabase(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := ProveSessionAffinity(ctx, conn, pool); err != nil {
		return nil, err
	}

	key := TableLockKey(database, schema, table)
	opCtx, cancel := context.WithTimeout(ctx, lockOperationTimeout)
	defer cancel()
	var taken bool
	if err := conn.QueryRow(opCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&taken); err != nil {
		return nil, fmt.Errorf("take the advisory lock for %s.%s: %w", schema, table, err)
	}
	if !taken {
		return nil, fmt.Errorf("%w: %s.%s", ErrTableLocked, schema, table)
	}
	// INV: LK-1 — the acquisition is confirmed against the catalog, not
	// inferred from the statement's own answer. This is the same reading
	// the keepalive repeats, so the lock starts its life having passed the
	// check that will end it.
	confirmed, err := sessionHoldsAdvisoryLock(opCtx, conn, key)
	if err != nil {
		return nil, err
	}
	if !confirmed {
		return nil, fmt.Errorf("%w: the advisory lock for %s.%s was granted on a session this connection does not reach",
			ErrNoSessionAffinity, schema, table)
	}

	l := &TableLock{
		schema:  schema,
		table:   table,
		key:     key,
		pid:     conn.Conn().PgConn().PID(),
		pool:    lockPool,
		conn:    conn,
		logger:  logger,
		lost:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	// The keepalive outlives the acquiring call's context: the lock is held
	// until Release, whatever the caller does with the context it acquired
	// under.
	keepaliveCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	l.stop = stop
	go l.keepalive(keepaliveCtx, interval)

	held = true
	logger.Debug("table lock acquired",
		"schema", schema, "table", table, "database", database, "backend_pid", l.pid)
	return l, nil
}

// Schema returns the locked table's schema.
func (l *TableLock) Schema() string { return l.schema }

// Table returns the locked table's name.
func (l *TableLock) Table() string { return l.table }

// BackendPID returns the pid of the backend holding the lock, the handle an
// operator needs to find it in pg_stat_activity.
func (l *TableLock) BackendPID() uint32 { return l.pid }

// Guard confirms the lock is held right now and returns a context that is
// cancelled if it is ever lost, so work started under it stops rather than
// continuing unprotected. The caller must call the returned cancel function.
//
// The confirmation is a catalog read on the lock's own session: a caller
// holding a TableLock value is not taken as evidence that the lock survived
// since it was minted.
//
// INV: LK-1 — losing the lock is fail-closed.
func (l *TableLock) Guard(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := l.Confirm(ctx); err != nil {
		return nil, nil, err
	}
	guarded, cancel := context.WithCancelCause(ctx)
	// The watcher's lifetime is the guarded operation's: it ends when the
	// lock is lost, when the caller cancels, or when the parent context
	// does.
	go func() {
		select {
		case <-l.lost:
			cancel(l.LostCause())
		case <-guarded.Done():
		}
	}()
	return guarded, func() { cancel(context.Canceled) }, nil
}

// Confirm reports whether the lock is still held, reading the catalog on the
// lock's session. A negative or failed reading marks the lock lost, so the
// answer and every guarded context agree.
func (l *TableLock) Confirm(ctx context.Context) error {
	if err := l.LostCause(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, keepaliveTimeout)
	defer cancel()

	l.connMu.Lock()
	defer l.connMu.Unlock()
	held, err := sessionHoldsAdvisoryLock(ctx, l.conn, l.key)
	if err != nil {
		// A reading that could not complete says nothing about the lock,
		// and "says nothing" is not "still held".
		l.markLost(fmt.Errorf("%w: confirming the lock for %s.%s failed: %w", ErrLockLost, l.schema, l.table, err))
		return l.LostCause()
	}
	if !held {
		l.markLost(fmt.Errorf("%w: the advisory lock for %s.%s is not held by backend %d",
			ErrLockLost, l.schema, l.table, l.pid))
		return l.LostCause()
	}
	return nil
}

// LostCause returns why the lock was lost, or nil while it is held.
func (l *TableLock) LostCause() error {
	l.lostMu.Lock()
	defer l.lostMu.Unlock()
	return l.lostCause
}

// Release stops the keepalive, releases the lock, and closes the connection
// that held it.
//
// A release the server reports as a no-op is returned as ErrLockLost rather
// than as a routine answer: the caller believed it held the lock, so the
// server disagreeing is the signal that its exclusion had already gone. The
// connection and its pool are closed either way, which ends the session and
// drops anything still on it.
func (l *TableLock) Release(ctx context.Context) error {
	var err error
	l.released.Do(func() {
		l.stop()
		<-l.stopped
		err = l.releaseLock(ctx)
		closeLockPool(l.conn, l.pool)
	})
	return err
}

// closeLockPool ends the lock session. The connection goes back to its pool
// first because a pool closes only once its connections are returned, and
// the pool is the lock's alone: closing it ends the session, which releases
// anything still held on it.
func closeLockPool(conn *pgxpool.Conn, pool *pgxpool.Pool) {
	conn.Release()
	pool.Close()
}

// releaseLock issues the unlock on the lock's session and reports a
// disagreement about ownership.
func (l *TableLock) releaseLock(ctx context.Context) error {
	if l.LostCause() != nil {
		// The lock is already known to be gone; the unlock below would only
		// confirm it, and the caller has been told once already.
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockOperationTimeout)
	defer cancel()

	l.connMu.Lock()
	defer l.connMu.Unlock()
	var released bool
	if err := l.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", l.key).Scan(&released); err != nil {
		return fmt.Errorf("release the advisory lock for %s.%s: %w", l.schema, l.table, err)
	}
	if !released {
		return fmt.Errorf("%w: releasing the lock for %s.%s reported it was not held by backend %d",
			ErrLockLost, l.schema, l.table, l.pid)
	}
	l.logger.Debug("table lock released", "schema", l.schema, "table", l.table, "backend_pid", l.pid)
	return nil
}

// keepalive re-confirms the lock on an interval until the lock is released
// or found lost. The confirmation doubles as the session's traffic, so the
// same statement that proves the lock is held is what keeps the session from
// looking idle.
func (l *TableLock) keepalive(ctx context.Context, interval time.Duration) {
	defer close(l.stopped)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.lost:
			return
		case <-ticker.C:
			if err := l.Confirm(ctx); err != nil {
				if ctx.Err() != nil {
					// The lock is being released; a confirmation cut short
					// by that is not a lost lock.
					return
				}
				l.logger.Error("the table lock was lost; the schema change under it will stop",
					"schema", l.schema, "table", l.table, "backend_pid", l.pid, "error", err)
				return
			}
		}
	}
}

// markLost records the first cause and closes the lost channel, cancelling
// every guarded context.
func (l *TableLock) markLost(cause error) {
	l.lostOnce.Do(func() {
		l.lostMu.Lock()
		l.lostCause = cause
		l.lostMu.Unlock()
		close(l.lost)
	})
}

// TableLockKey derives the advisory-lock key for one table. It is a
// cross-process, cross-version contract: two instances running different
// builds must derive the same key for the same table or they will not
// exclude each other, so the derivation is pinned by test.
func TableLockKey(database, schema, table string) int64 {
	h := fnv.New64a()
	// The separator cannot appear in an identifier, so no two distinct
	// (database, schema, table) triples produce the same input.
	for _, part := range []string{database, schema, table} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64())
}

// newLockPool builds the dedicated single-connection pool the lock lives on.
// It inherits the caller's connection settings — host, credentials, TLS, the
// bounded session timeouts — and narrows the pool to one connection.
//
// The lock's exemption from connection recycling is structural rather than
// configured: pgxpool enforces a connection's lifetime and idle time when it
// hands a connection out and when it sweeps idle ones, and the lock holds
// its connection checked out from acquisition until release, so neither
// path can reach it. Turning the limits off instead would be the fragile
// way to say the same thing — a zero lifetime is not "unlimited" to
// pgxpool, it is "already expired".
func newLockPool(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Pool, error) {
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	// The lock session is identifiable in pg_stat_activity, because "which
	// backend holds this lock" is the first question a blocked operator asks.
	cfg.ConnConfig.RuntimeParams["application_name"] = "pg-sprite (table lock)"
	lockPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create the table lock pool: %w", err)
	}
	return lockPool, nil
}

// currentDatabase reads the database the lock connection is attached to. The
// key is derived from it rather than from the connection string, so two
// instances reaching one database by different routes still collide on the
// same key.
func currentDatabase(ctx context.Context, conn *pgxpool.Conn) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lockOperationTimeout)
	defer cancel()
	var database string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		return "", fmt.Errorf("read the current database: %w", err)
	}
	return database, nil
}
