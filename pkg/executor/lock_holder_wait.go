package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// lockHolderSampleGap is how far apart two reads of a table's lock holders
// are taken. A transaction seen holding a lock in both reads has held it at
// least this long, so an attempt entering the lock queue now would wait on
// it, and every session that touches the table after the attempt would queue
// behind the attempt for as long. A transaction that commits within the gap
// is the ordinary traffic a lock budget exists to absorb.
const lockHolderSampleGap = 200 * time.Millisecond

// DefaultHolderWait is the longest DefaultRetryPolicy waits outside the lock
// queue for a transaction holding a lock that reads or writes take on the
// table. It is long enough for a slow report or a batch job's transaction to
// finish, and short enough that a step held up by an idle-in-transaction
// session reaches its own lock budget, and the error that names the cause,
// quickly.
const DefaultHolderWait = 30 * time.Second

// readLockModes and writeLockModes are the table lock modes, as pg_locks
// names them, that application reads (SELECT, SELECT ... FOR UPDATE) and
// writes (INSERT, UPDATE, DELETE, MERGE) take. Only these holders are waited
// out: a transaction holding a stronger mode is another schema change or
// maintenance command, which waiting cannot outlast, and autovacuum gives way
// to a lock request on its own.
func readLockModes() []string  { return []string{"AccessShareLock", "RowShareLock"} }
func writeLockModes() []string { return []string{"RowExclusiveLock"} }

// readAndWriteLockModes are the modes ACCESS EXCLUSIVE conflicts with among
// those reads and writes take.
func readAndWriteLockModes() []string { return append(readLockModes(), writeLockModes()...) }

// lockedTable is one table a step locks and the holders' modes that conflict
// with the lock it takes there.
type lockedTable struct {
	schema, table string
	// descendants is set when the step also locks the table's partitions
	// and inheritance children.
	descendants bool
	modes       []string
}

// nativeLockedTables reads, from the statement, the tables a native step
// locks and which holders conflict on each.
//
// An ALTER TABLE whose every subcommand takes only SHARE UPDATE EXCLUSIVE,
// such as VALIDATE CONSTRAINT, conflicts with no read or write and waits on
// nothing. One that only adds foreign keys takes SHARE ROW EXCLUSIVE, which
// conflicts with writes alone. Every other statement is treated as taking
// ACCESS EXCLUSIVE, so the wait can be longer than it needs to be, never
// shorter. An ALTER TABLE that names its table with ONLY locks that table
// alone; any other statement locks the descendants too. A foreign key the
// statement adds also takes SHARE ROW EXCLUSIVE on the table it references,
// resolved in schema when the statement leaves it unqualified, as the
// attempt's search_path does.
func nativeLockedTables(st statement.Statement, schema, table string) ([]lockedTable, error) {
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return nil, fmt.Errorf("read the locks %q takes: %w", st.SQL(), err)
	}
	target := lockedTable{schema: schema, table: table, descendants: true, modes: readAndWriteLockModes()}
	if len(ops) > 0 {
		shareUpdateExclusiveOnly, foreignKeysOnly, only := true, true, true
		for _, op := range ops {
			shareUpdateExclusiveOnly = shareUpdateExclusiveOnly && op.TakesShareUpdateExclusiveOnly()
			foreignKeysOnly = foreignKeysOnly && op.Kind == statement.OpAddConstraint && op.Constraint == statement.ConstraintForeignKey
			only = only && op.Only
		}
		if shareUpdateExclusiveOnly {
			return nil, nil
		}
		if foreignKeysOnly {
			target.modes = writeLockModes()
		}
		target.descendants = !only
	}
	return withReferencedTables(st, schema, []lockedTable{target})
}

// withReferencedTables adds the tables the statement's foreign keys
// reference, other than those already listed, each waited on for writers.
func withReferencedTables(st statement.Statement, schema string, tables []lockedTable) ([]lockedTable, error) {
	refs, err := statement.ForeignKeyReferencedTables(st.SQL())
	if err != nil {
		return nil, fmt.Errorf("read the tables %q references: %w", st.SQL(), err)
	}
	for _, ref := range refs {
		if ref.Schema == "" {
			ref.Schema = schema
		}
		listed := slices.ContainsFunc(tables, func(t lockedTable) bool { return t.schema == ref.Schema && t.table == ref.Name })
		if !listed {
			tables = append(tables, lockedTable{schema: ref.Schema, table: ref.Name, descendants: true, modes: writeLockModes()})
		}
	}
	return tables, nil
}

// lockHolder is one transaction holding a conflicting lock.
type lockHolder struct {
	pid   int32
	table string
	// since is the wait elapsed when the transaction was first seen.
	since time.Duration
}

// holderWaitOutcome is how a wait outside the lock queue ended.
type holderWaitOutcome struct {
	// exhausted is set when a holder was still holding its lock once the
	// limit ran out, and holder names the one that had held it longest.
	exhausted bool
	holder    lockHolder
	waited    time.Duration
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

// trackerHolderWaitReport publishes the wait on tracker, which may be nil.
func trackerHolderWaitReport(tracker *progress.Tracker) func(*progress.LockHolderWait) {
	return func(wait *progress.LockHolderWait) {
		if tracker != nil {
			tracker.SetLockHolderWait(wait)
		}
	}
}

// waitOutsideLockQueue returns once no transaction has held a conflicting
// lock on any of the tables, or on a descendant the step also locks, across
// two reads lockHolderSampleGap apart, or once one has for limit. Either way
// the caller then requests its lock as it would without the wait, under its
// own lock budget, so the wait can delay a step but never fails one. A zero
// limit, or no tables, returns at once.
//
// While a holder lingers, report receives the holder that has held its lock
// longest and the time waited; it receives nil when the wait ends.
//
// One deadline bounds the whole wait, resolving the tables and every read of
// their holders included, and no sleep runs past it, so the wait never
// outlasts limit by more than one catalog round trip. The reads take no lock
// and use pg_locks alone, which every role can read in full, so the wait does
// not depend on the privilege to see other roles' sessions in
// pg_stat_activity. Every operator and function is qualified with
// pg_catalog, so a caller-owned pool whose search_path puts another schema
// first cannot hide a holder (CO-9). A prepared transaction has no session to
// finish it, so it is not waited on. A table the name no longer resolves to
// is skipped: the attempt that follows reports it.
func waitOutsideLockQueue(ctx context.Context, pool *pgxpool.Pool, tables []lockedTable, limit time.Duration, clock holderWaitClock, report func(*progress.LockHolderWait)) (holderWaitOutcome, error) {
	if limit == 0 || len(tables) == 0 {
		return holderWaitOutcome{}, nil
	}
	start := clock.now()
	remaining := func() time.Duration { return limit - clock.now().Sub(start) }
	reported := false
	defer func() {
		if reported {
			report(nil)
		}
	}()
	exhausted := func(oldest lockHolder) holderWaitOutcome {
		return holderWaitOutcome{exhausted: true, holder: oldest, waited: clock.now().Sub(start)}
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

	relations := make(map[uint32]lockedTable)
	for _, t := range tables {
		timedOut, err := bounded(func(ctx context.Context) error {
			oids, err := lockedRelations(ctx, pool, t.schema, t.table, t.descendants)
			for _, oid := range oids {
				relations[oid] = t
			}
			return err
		})
		if err != nil {
			return holderWaitOutcome{}, err
		}
		if timedOut {
			return exhausted(lockHolder{}), nil
		}
	}
	if len(relations) == 0 {
		return holderWaitOutcome{}, nil
	}
	sample := func() (map[string]lockHolder, bool, error) {
		var seen map[string]lockHolder
		timedOut, err := bounded(func(ctx context.Context) error {
			var err error
			seen, err = conflictingLockHolders(ctx, pool, relations)
			return err
		})
		if err != nil {
			return nil, false, fmt.Errorf("read lock holders on %s: %w", qualifiedName(tables[0].schema, tables[0].table), err)
		}
		return seen, timedOut, nil
	}

	holders, timedOut, err := sample()
	if err != nil {
		return holderWaitOutcome{}, err
	}
	if timedOut {
		return exhausted(lockHolder{}), nil
	}
	oldest := oldestHolder(holders)
	for len(holders) > 0 {
		left := remaining()
		if left <= 0 {
			return exhausted(oldest), nil
		}
		if err := clock.sleep(ctx, min(lockHolderSampleGap, left)); err != nil {
			return holderWaitOutcome{}, err
		}
		waited := clock.now().Sub(start)
		seen, timedOut, err := sample()
		if err != nil {
			return holderWaitOutcome{}, err
		}
		if timedOut {
			return exhausted(oldest), nil
		}
		lingering := false
		next := make(map[string]lockHolder, len(seen))
		for vxid, holder := range seen {
			if earlier, held := holders[vxid]; held {
				holder.since = earlier.since
				lingering = true
			} else {
				holder.since = waited
			}
			next[vxid] = holder
		}
		if !lingering {
			return holderWaitOutcome{waited: waited}, nil
		}
		holders = next
		oldest = oldestHolder(holders)
		report(&progress.LockHolderWait{PID: uint32(oldest.pid), Table: oldest.table, Waited: waited, Limit: limit})
		reported = true
	}
	return holderWaitOutcome{waited: clock.now().Sub(start)}, nil
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

// lockedRelations resolves the OIDs a step locks for one table: the table,
// resolved the way the attempt's statement resolves its name
// (schema-qualified when the proof carries a schema, on the session
// search_path otherwise), and, when descendants is set, every partition and
// inheritance child below it. A name that resolves to nothing returns no
// OIDs.
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

// conflictingLockHolders returns, by virtual transaction ID, the sessions
// other than this one holding a granted lock on one of the relations in a
// mode that conflicts there, each with its backend's PID and the table it was
// resolved from.
func conflictingLockHolders(ctx context.Context, pool *pgxpool.Pool, relations map[uint32]lockedTable) (map[string]lockHolder, error) {
	oids := make([]uint32, 0, len(relations))
	for oid := range relations {
		oids = append(oids, oid)
	}
	rows, err := pool.Query(ctx, `
		SELECT l.virtualtransaction, l.pid, l.relation, l.mode
		FROM pg_catalog.pg_locks l
		WHERE l.locktype OPERATOR(pg_catalog.=) 'relation'
		  AND l.database OPERATOR(pg_catalog.=) (
			SELECT d.oid FROM pg_catalog.pg_database d
			WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database())
		  AND l.relation OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.oid[])
		  AND l.mode OPERATOR(pg_catalog.=) ANY ($2::pg_catalog.text[])
		  AND l.granted
		  AND l.pid IS NOT NULL
		  AND l.pid OPERATOR(pg_catalog.<>) pg_catalog.pg_backend_pid()`, oids, readAndWriteLockModes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	holders := make(map[string]lockHolder)
	for rows.Next() {
		var vxid, mode string
		var pid int32
		var relation uint32
		if err := rows.Scan(&vxid, &pid, &relation, &mode); err != nil {
			return nil, err
		}
		t := relations[relation]
		if !slices.Contains(t.modes, mode) {
			continue
		}
		holders[vxid] = lockHolder{pid: pid, table: qualifiedName(t.schema, t.table)}
	}
	return holders, rows.Err()
}
