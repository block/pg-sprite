package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lockHolderSampleGap is how far apart two reads of a table's lock holders
// are taken. A transaction seen holding a lock in both reads has held it at
// least this long, so an attempt entering the lock queue now would wait on
// it, and every session that touches the table after the attempt would queue
// behind the attempt for as long. A transaction that commits within the gap
// is the ordinary traffic a lock budget exists to absorb.
const lockHolderSampleGap = 200 * time.Millisecond

// LockHolderWaitError reports that a transaction held a lock on the target
// table for the retry policy's whole HolderWait. The attempt never entered
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

// waitOutsideLockQueue returns once no transaction has held a lock on the
// table across two reads lockHolderSampleGap apart, or a *LockHolderWaitError
// once one has for limit. A zero limit returns at once. The reads use
// pg_locks alone, which every role can read in full, so the wait does not
// depend on the privilege to see other roles' sessions in
// pg_stat_activity. A table the name no longer resolves to returns at once:
// the attempt that follows reports it.
func waitOutsideLockQueue(ctx context.Context, pool *pgxpool.Pool, schema, table string, limit time.Duration, sleep sleepFunc) error {
	if limit == 0 {
		return nil
	}
	relation, found, err := tableRelation(ctx, pool, schema, table)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	seen, err := tableLockHolders(ctx, pool, relation)
	if err != nil {
		return fmt.Errorf("read lock holders on %s: %w", qualifiedName(schema, table), err)
	}
	holders := make(map[string]lockHolder, len(seen))
	for vxid, pid := range seen {
		holders[vxid] = lockHolder{pid: pid}
	}
	var waited time.Duration
	for len(holders) > 0 {
		if err := sleep(ctx, lockHolderSampleGap); err != nil {
			return err
		}
		waited += lockHolderSampleGap
		seen, err := tableLockHolders(ctx, pool, relation)
		if err != nil {
			return fmt.Errorf("read lock holders on %s: %w", qualifiedName(schema, table), err)
		}
		lingering, oldest := false, lockHolder{since: waited}
		next := make(map[string]lockHolder, len(seen))
		for vxid, pid := range seen {
			holder, held := holders[vxid]
			if !held {
				next[vxid] = lockHolder{pid: pid, since: waited}
				continue
			}
			next[vxid] = holder
			lingering = true
			if holder.since < oldest.since {
				oldest = holder
			}
		}
		if !lingering {
			return nil
		}
		if waited >= limit {
			return &LockHolderWaitError{Schema: schema, Table: table, PID: oldest.pid, Waited: waited}
		}
		holders = next
	}
	return nil
}

// tableRelation resolves the table's OID the way the attempt's statement
// resolves its name: schema-qualified when the proof carries a schema, on the
// session search_path otherwise.
func tableRelation(ctx context.Context, pool *pgxpool.Pool, schema, table string) (uint32, bool, error) {
	var relation *uint32
	err := pool.QueryRow(ctx, `
		SELECT pg_catalog.to_regclass(CASE
			WHEN $1 = '' THEN pg_catalog.quote_ident($2)
			ELSE pg_catalog.quote_ident($1) || '.' || pg_catalog.quote_ident($2)
		END)::pg_catalog.oid`, schema, table).Scan(&relation)
	if err != nil {
		return 0, false, fmt.Errorf("resolve %s for its lock holders: %w", qualifiedName(schema, table), err)
	}
	if relation == nil {
		return 0, false, nil
	}
	return *relation, true, nil
}

// tableLockHolders returns the transactions holding a granted lock on the
// relation, by virtual transaction ID, with their backend's PID. Every lock
// mode counts: the native statements take ACCESS EXCLUSIVE, which conflicts
// with all of them.
func tableLockHolders(ctx context.Context, pool *pgxpool.Pool, relation uint32) (map[string]int32, error) {
	rows, err := pool.Query(ctx, `
		SELECT l.virtualtransaction, COALESCE(l.pid, 0)
		FROM pg_catalog.pg_locks l
		WHERE l.locktype = 'relation'
		  AND l.database = (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database())
		  AND l.relation = $1
		  AND l.granted
		  AND l.pid IS DISTINCT FROM pg_catalog.pg_backend_pid()`, relation)
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
