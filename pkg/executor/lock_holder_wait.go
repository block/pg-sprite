package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// lockHolderSampleGap is how far apart two reads of a table's lock holders
// are taken. A transaction seen holding a lock in both reads has held it at
// least this long, so an attempt entering the lock queue now would wait on
// it, and every session that touches the table after the attempt would queue
// behind the attempt for as long. A transaction that commits within the gap
// is the ordinary traffic a lock budget exists to absorb.
const lockHolderSampleGap = 200 * time.Millisecond

// LockHolderWaitError reports that a transaction held a lock conflicting with
// the attempt's on the target table, or on a partition or child the attempt
// also locks, for the retry policy's whole HolderWait. The attempt never entered
// the lock queue, so nothing was submitted and no session queued behind it.
type LockHolderWaitError struct {
	Schema string
	Table  string
	// PID is the backend of the transaction that held its lock longest, or
	// 0 for a prepared transaction, which has no backend.
	PID int32
	// Waited is how long the attempt waited outside the lock queue.
	Waited time.Duration
}

// Code reports the error's stable outcome code.
func (e *LockHolderWaitError) Code() Code { return CodeLockHolderWaitExceeded }

func (e *LockHolderWaitError) Error() string {
	return fmt.Sprintf("a transaction (pid %d) held a lock on %s for the whole %s the attempt waited outside the lock queue; nothing was submitted",
		e.PID, qualifiedName(e.Schema, e.Table), e.Waited)
}

// lockHolder is one transaction holding a granted lock on the table.
type lockHolder struct {
	pid int32
	// since is the wait elapsed when the transaction was first seen.
	since time.Duration
}

// everyLockMode lists PostgreSQL's table lock modes as pg_locks names them.
// Every one conflicts with ACCESS EXCLUSIVE, which most native statements
// take.
func everyLockMode() []string {
	return []string{
		"AccessShareLock", "RowShareLock", "RowExclusiveLock", "ShareUpdateExclusiveLock",
		"ShareLock", "ShareRowExclusiveLock", "ExclusiveLock", "AccessExclusiveLock",
	}
}

// shareUpdateExclusiveConflicts lists the modes that conflict with SHARE
// UPDATE EXCLUSIVE. Readers and writers take none of them, so they never hold
// up an attempt that takes only that lock.
func shareUpdateExclusiveConflicts() []string {
	return []string{
		"ShareUpdateExclusiveLock", "ShareLock", "ShareRowExclusiveLock", "ExclusiveLock", "AccessExclusiveLock",
	}
}

// holderScope is which lock holders an attempt waits out: the modes that
// conflict with the lock the statement requests, and whether the statement
// also locks the table's partitions and inheritance children.
type holderScope struct {
	modes       []string
	descendants bool
}

// holderScopeOf reads the scope from the statement. An ALTER TABLE whose every
// subcommand takes only SHARE UPDATE EXCLUSIVE, such as VALIDATE CONSTRAINT,
// waits only on the modes that conflict with that lock; every other statement
// is treated as taking ACCESS EXCLUSIVE, so the wait can only be longer than
// it needs to be, never shorter. An ALTER TABLE that names its table with
// ONLY locks that table alone; any other statement locks the descendants too.
func holderScopeOf(st statement.Statement) (holderScope, error) {
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return holderScope{}, fmt.Errorf("read the lock %q requests: %w", st.SQL(), err)
	}
	scope := holderScope{modes: everyLockMode(), descendants: true}
	if len(ops) == 0 {
		return scope, nil
	}
	shareUpdateExclusiveOnly, only := true, true
	for _, op := range ops {
		shareUpdateExclusiveOnly = shareUpdateExclusiveOnly && op.TakesShareUpdateExclusiveOnly()
		only = only && op.Only
	}
	if shareUpdateExclusiveOnly {
		scope.modes = shareUpdateExclusiveConflicts()
	}
	scope.descendants = !only
	return scope, nil
}

// holderWaitClock is the time source a wait measures itself against.
type holderWaitClock struct {
	now   func() time.Time
	sleep sleepFunc
}

// systemHolderWaitClock is the wall clock and a context-aware sleep.
func systemHolderWaitClock() holderWaitClock {
	return holderWaitClock{now: time.Now, sleep: sleepContext}
}

// waitOutsideLockQueue returns once no transaction has held a conflicting
// lock on the table, or on a descendant the statement also locks, across two
// reads lockHolderSampleGap apart, or a *LockHolderWaitError once one has for
// limit. A zero limit returns at once.
//
// One deadline bounds the whole wait, resolving the table and every read of
// its holders included, and no sleep runs past it, so the wait never outlasts
// limit by more than one catalog round trip. The reads use pg_locks alone,
// which every role can read in full, so the wait does not depend on the
// privilege to see other roles' sessions in pg_stat_activity. Every operator
// and function is qualified with pg_catalog, so a caller-owned pool whose
// search_path puts another schema first cannot hide a holder (CO-9). A table
// the name no longer resolves to returns at once: the attempt that follows
// reports it.
func waitOutsideLockQueue(ctx context.Context, pool *pgxpool.Pool, st statement.Statement, schema, table string, limit time.Duration, clock holderWaitClock) error {
	if limit == 0 {
		return nil
	}
	scope, err := holderScopeOf(st)
	if err != nil {
		return err
	}
	start := clock.now()
	remaining := func() time.Duration { return limit - clock.now().Sub(start) }
	exhausted := func(oldest lockHolder) error {
		return &LockHolderWaitError{Schema: schema, Table: table, PID: oldest.pid, Waited: clock.now().Sub(start)}
	}
	// bounded runs one catalog read under the wait's remaining budget and
	// reports whether the budget, not the caller, ended it.
	bounded := func(read func(context.Context) error) (bool, error) {
		readCtx, cancel := context.WithTimeout(ctx, remaining())
		defer cancel()
		err := read(readCtx)
		if err != nil && ctx.Err() == nil && errors.Is(readCtx.Err(), context.DeadlineExceeded) {
			return true, nil
		}
		return false, err
	}

	var relations []uint32
	timedOut, err := bounded(func(ctx context.Context) error {
		var err error
		relations, err = lockedRelations(ctx, pool, schema, table, scope.descendants)
		return err
	})
	if err != nil {
		return err
	}
	if timedOut {
		return exhausted(lockHolder{})
	}
	if len(relations) == 0 {
		return nil
	}
	sample := func() (map[string]int32, bool, error) {
		var seen map[string]int32
		timedOut, err := bounded(func(ctx context.Context) error {
			var err error
			seen, err = tableLockHolders(ctx, pool, relations, scope.modes)
			return err
		})
		if err != nil {
			return nil, false, fmt.Errorf("read lock holders on %s: %w", qualifiedName(schema, table), err)
		}
		return seen, timedOut, nil
	}

	seen, timedOut, err := sample()
	if err != nil {
		return err
	}
	if timedOut {
		return exhausted(lockHolder{})
	}
	holders := make(map[string]lockHolder, len(seen))
	for vxid, pid := range seen {
		holders[vxid] = lockHolder{pid: pid}
	}
	oldest := oldestHolder(holders)
	for len(holders) > 0 {
		left := remaining()
		if left <= 0 {
			return exhausted(oldest)
		}
		if err := clock.sleep(ctx, min(lockHolderSampleGap, left)); err != nil {
			return err
		}
		waited := clock.now().Sub(start)
		seen, timedOut, err := sample()
		if err != nil {
			return err
		}
		if timedOut {
			return exhausted(oldest)
		}
		lingering := false
		next := make(map[string]lockHolder, len(seen))
		for vxid, pid := range seen {
			holder, held := holders[vxid]
			if !held {
				next[vxid] = lockHolder{pid: pid, since: waited}
				continue
			}
			next[vxid] = holder
			lingering = true
		}
		if !lingering {
			return nil
		}
		holders = next
		oldest = oldestHolder(holders)
	}
	return nil
}

// oldestHolder returns the holder first seen earliest.
func oldestHolder(holders map[string]lockHolder) lockHolder {
	var oldest lockHolder
	first := true
	for _, holder := range holders {
		if first || holder.since < oldest.since {
			oldest, first = holder, false
		}
	}
	return oldest
}

// lockedRelations resolves the OIDs the attempt locks: the table, resolved
// the way the attempt's statement resolves its name (schema-qualified when the
// proof carries a schema, on the session search_path otherwise), and, when
// descendants is set, every partition and inheritance child below it. A name
// that resolves to nothing returns no OIDs.
func lockedRelations(ctx context.Context, pool *pgxpool.Pool, schema, table string, descendants bool) ([]uint32, error) {
	rows, err := pool.Query(ctx, `
		WITH RECURSIVE target(relid) AS (
			SELECT pg_catalog.to_regclass(CASE
				WHEN $1::pg_catalog.text OPERATOR(pg_catalog.=) '' THEN pg_catalog.quote_ident($2::pg_catalog.text)
				ELSE pg_catalog.quote_ident($1::pg_catalog.text) OPERATOR(pg_catalog.||) '.'
					OPERATOR(pg_catalog.||) pg_catalog.quote_ident($2::pg_catalog.text)
			END)::pg_catalog.oid
		), tree(relid) AS (
			SELECT relid FROM target WHERE relid IS NOT NULL
			UNION
			SELECT i.inhrelid
			FROM pg_catalog.pg_inherits i
			JOIN tree t ON i.inhparent OPERATOR(pg_catalog.=) t.relid
			WHERE $3::pg_catalog.bool
		)
		SELECT relid FROM tree`, schema, table, descendants)
	if err != nil {
		return nil, fmt.Errorf("resolve %s for its lock holders: %w", qualifiedName(schema, table), err)
	}
	defer rows.Close()
	var relations []uint32
	for rows.Next() {
		var relation uint32
		if err := rows.Scan(&relation); err != nil {
			return nil, fmt.Errorf("resolve %s for its lock holders: %w", qualifiedName(schema, table), err)
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve %s for its lock holders: %w", qualifiedName(schema, table), err)
	}
	return relations, nil
}

// tableLockHolders returns the transactions holding a granted lock in one of
// modes on any of the relations, by virtual transaction ID, with their
// backend's PID. A prepared transaction has no backend and reports 0.
func tableLockHolders(ctx context.Context, pool *pgxpool.Pool, relations []uint32, modes []string) (map[string]int32, error) {
	rows, err := pool.Query(ctx, `
		SELECT l.virtualtransaction, COALESCE(l.pid, 0)
		FROM pg_catalog.pg_locks l
		WHERE l.locktype OPERATOR(pg_catalog.=) 'relation'
		  AND l.database OPERATOR(pg_catalog.=) (
			SELECT d.oid FROM pg_catalog.pg_database d
			WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database())
		  AND l.relation OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.oid[])
		  AND l.mode OPERATOR(pg_catalog.=) ANY ($2::pg_catalog.text[])
		  AND l.granted
		  AND (l.pid IS NULL OR l.pid OPERATOR(pg_catalog.<>) pg_catalog.pg_backend_pid())`, relations, modes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	holders := make(map[string]int32)
	for rows.Next() {
		var vxid string
		var pid int32
		if err := rows.Scan(&vxid, &pid); err != nil {
			return nil, err
		}
		holders[vxid] = pid
	}
	return holders, rows.Err()
}
