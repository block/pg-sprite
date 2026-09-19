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

// TableLockClassID is the high half of every pg-sprite table lock. The
// two-argument advisory lock form puts it in pg_locks.classid, which gives
// pg-sprite a keyspace of its own inside the database's shared advisory
// keyspace: an unrelated application's single-argument key can never equal
// a table lock, so a spurious contention refusal can only come from two
// tables whose names hash alike. The value spells "pgsp".
const TableLockClassID int32 = 0x70677370

// tableLockObjIDSQL derives the low half of a table lock's key on the server
// from the bound schema ($1) and table ($2). quote_ident makes the input
// unambiguous — schema "odd.schema" with table "t" and schema "odd" with
// table "schema.t" render differently — and it is the form an operator can
// paste into a runbook query. hashtext is a property of the server rather
// than of the string, so the key is reproducible only by asking the same
// server, which is why it is never derived client-side.
const tableLockObjIDSQL = "hashtext(quote_ident($1) || '.' || quote_ident($2))"

// ErrInvariantViolation is returned when a dbconn proof cannot be constructed safely.
var ErrInvariantViolation = errors.New("invariant violation")

// TableLockKey is the (classid, objid) pair pg_locks reports for a table
// lock, as PostgreSQL's two-argument advisory lock functions take it.
type TableLockKey struct {
	ClassID int32
	ObjID   int32
}

// TableLockHolder describes the session holding a table lock, as pg_locks
// joined to pg_stat_activity reports it. ApplicationName and State are
// empty, and BackendStart zero, when pg_stat_activity hides the session's
// details from the connected role.
type TableLockHolder struct {
	PID             uint32
	ApplicationName string
	BackendStart    time.Time
	State           string
}

// TableLockHeldError reports that another session holds a table's advisory
// lock. Holder names that session when the catalog answered the lookup;
// its zero value means the lock was released again before the lookup ran,
// or the lookup itself failed (the failure is joined to this error).
type TableLockHeldError struct {
	Schema string
	Table  string
	Holder TableLockHolder
}

func (e *TableLockHeldError) Error() string {
	msg := fmt.Sprintf("table lock is already held for %s", tableLockQualifiedName(e.Schema, e.Table))
	if e.Holder.PID == 0 {
		return msg
	}
	msg += fmt.Sprintf(" by backend %d", e.Holder.PID)
	if e.Holder.ApplicationName != "" {
		msg += fmt.Sprintf(" (application_name %q", e.Holder.ApplicationName)
		if !e.Holder.BackendStart.IsZero() {
			msg += fmt.Sprintf(", connected since %s", e.Holder.BackendStart.UTC().Format(time.RFC3339))
		}
		msg += ")"
	}
	return msg
}

// TableLock proves this process holds LK-1's per-table advisory lock on a
// dedicated session. Its zero value is forgeable; consumers must reject it
// when Table is empty.
type TableLock struct {
	schema, table string
	key           TableLockKey
}

// Schema returns the locked table's schema.
func (l TableLock) Schema() string { return l.schema }

// Table returns the locked table name.
func (l TableLock) Table() string { return l.table }

// Key returns the advisory lock key as pg_locks reports it.
func (l TableLock) Key() TableLockKey { return l.key }

// TableLockOption configures lock-session monitoring.
type TableLockOption func(*tableLockOptions) error

type tableLockOptions struct {
	keepalive time.Duration
}

// WithTableLockKeepalive sets the interval between lock confirmations. Each
// confirmation is bounded by the same interval, so a lost lock is reported
// on Done within two intervals of the loss. The interval must also stay
// under any idle_session_timeout the server enforces, since the
// confirmation traffic is what keeps the dedicated session from being idle.
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

// AcquireTableLock opens a dedicated, non-recycling connection, proves it
// keeps one server session, and tries to acquire the per-table advisory
// lock. It never retries a held lock: contention returns a
// TableLockHeldError naming the holder when the catalog reports one, and
// the ordering policy for a plan that touches several tables belongs to the
// caller. ctx bounds acquisition only; Release or detected lock loss ends
// the acquired session. schema and table must be catalog names as stored,
// including case, rather than unquoted SQL identifiers that PostgreSQL has
// not yet folded.
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
	// INV: LK-1 — a session-scoped lock is exclusive only on a connection
	// that keeps one server session. The proof runs before the lock is
	// taken: a refusal after taking it would strand a real lock on a
	// backend this client can no longer address.
	if err := proveAffinityOn(ctx, conn, pc); err != nil {
		return nil, errors.Join(fmt.Errorf("prove table lock session affinity: %w", err), conn.Close(ctx))
	}

	qualified := tableLockQualifiedName(schema, table)
	var objID int32
	var held bool
	var pid uint32
	// INV: LK-1 — the server derives and attempts the key atomically on the
	// dedicated session, so no unprotected window follows a client-side hash.
	err = conn.QueryRow(ctx, `SELECT `+tableLockObjIDSQL+`,
		pg_try_advisory_lock($3, `+tableLockObjIDSQL+`), pg_backend_pid()`,
		schema, table, TableLockClassID).Scan(&objID, &held, &pid)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("acquire table lock for %s: %w", qualified, err), conn.Close(ctx))
	}
	key := TableLockKey{ClassID: TableLockClassID, ObjID: objID}
	if !held {
		heldErr := &TableLockHeldError{Schema: schema, Table: table}
		holder, found, lookupErr := lookupTableLockHolder(ctx, conn, key)
		if found {
			heldErr.Holder = holder
		}
		return nil, errors.Join(heldErr, lookupErr, conn.Close(ctx))
	}
	// INV: LK-1 — acquisition is not a grant until this same connection can
	// observe the session-scoped lock it reported taking.
	confirmed, err := sessionHoldsAdvisoryLock(ctx, conn, pairAdvisoryLock(key))
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

// LookupTableLockHolder reports the session holding a table's lock without
// taking it, so a read-only surface can answer "who holds this table" from
// any connection. found is false when nobody holds the lock.
func LookupTableLockHolder(ctx context.Context, conn AdvisoryLockHolder, schema, table string) (holder TableLockHolder, found bool, err error) {
	if schema == "" || table == "" {
		return TableLockHolder{}, false, fmt.Errorf("%w: LK-1: table lock lookup requires non-empty schema and table", ErrInvariantViolation)
	}
	var objID int32
	if err := conn.QueryRow(ctx, "SELECT "+tableLockObjIDSQL, schema, table).Scan(&objID); err != nil {
		return TableLockHolder{}, false, fmt.Errorf("derive table lock key for %s: %w", tableLockQualifiedName(schema, table), err)
	}
	return lookupTableLockHolder(ctx, conn, TableLockKey{ClassID: TableLockClassID, ObjID: objID})
}

func lookupTableLockHolder(ctx context.Context, conn AdvisoryLockHolder, key TableLockKey) (holder TableLockHolder, found bool, err error) {
	ref := pairAdvisoryLock(key)
	const query = `SELECT l.pid, COALESCE(a.application_name, ''), a.backend_start, COALESCE(a.state, '')
		  FROM pg_catalog.pg_locks l
		  LEFT JOIN pg_catalog.pg_stat_activity a ON a.pid = l.pid
		 WHERE l.locktype = 'advisory'
		   AND l.granted
		   AND l.objsubid = $1
		   AND l.classid::bigint = $2
		   AND l.objid::bigint = $3
		 ORDER BY l.pid
		 LIMIT 1`
	var pid int32
	var backendStart *time.Time
	err = conn.QueryRow(ctx, query, ref.objsubid, ref.classID, ref.objID).Scan(&pid, &holder.ApplicationName, &backendStart, &holder.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return TableLockHolder{}, false, nil
	}
	if err != nil {
		return TableLockHolder{}, false, fmt.Errorf("read the table lock holder from pg_locks: %w", err)
	}
	holder.PID = uint32(pid)
	if backendStart != nil {
		holder.BackendStart = *backendStart
	}
	return holder, true, nil
}

// tableLockQualifiedName renders schema.table for messages. It is not the
// lock key's input; the server derives that from the quoted identifiers.
func tableLockQualifiedName(schema, table string) string { return schema + "." + table }

func (s *TableLockSession) monitor(ctx context.Context, opts tableLockOptions) {
	ticker := time.NewTicker(opts.keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, opts.keepalive)
			s.opMu.Lock()
			held, err := sessionHoldsAdvisoryLock(checkCtx, s.conn, pairAdvisoryLock(s.lock.key))
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

// Bind derives a context from parent that is also cancelled when this
// session ends: with Err as the cause when the lock is lost, and with
// context.Canceled on Release. Work the lock protects runs under the
// returned context so a lost lock cancels every statement in flight instead
// of relying on the caller to watch Done. The returned stop function ends
// the watcher and cancels the context; callers defer it.
func (s *TableLockSession) Bind(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	var wg sync.WaitGroup
	wg.Go(func() {
		select {
		case <-s.done:
			cancel(s.Err())
		case <-ctx.Done():
		}
	})
	return ctx, func() {
		cancel(nil)
		wg.Wait()
	}
}

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
	err := s.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1, $2)", s.lock.key.ClassID, s.lock.key.ObjID).Scan(&unlocked)
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
