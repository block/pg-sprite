package preflight

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// The bounds of the usage read's transaction. It takes no lock on the
// table or any partition, and every statement is a catalog or statistics
// lookup, so neither bound is expected to be reached.
const (
	usageReadLockTimeout      = time.Second
	usageReadStatementTimeout = 10 * time.Second
)

// scanTimesSinceVersionNum is the first server_version_num whose table
// statistics record when the table was last scanned (last_seq_scan and
// last_idx_scan arrived in PostgreSQL 16).
const scanTimesSinceVersionNum = 160000

// The relkinds the usage read distinguishes.
const (
	relkindOrdinary    = "r"
	relkindPartitioned = "p"
	relkindForeign     = "f"
)

// LockHolder is one session, other than the reading one, that held a
// granted lock on the table — or, for a partitioned table, on the table or
// one of its partitions — when its usage was read. A session holding locks
// on several of those relations is one LockHolder per relation.
type LockHolder struct {
	// PID is the holder's backend process ID, or 0 when the holder is a
	// prepared transaction, which keeps its locks with no backend.
	PID int32
	// ApplicationName is the holder's application_name, empty when it set
	// none or for a prepared transaction.
	ApplicationName string
	// Modes are the lock modes the holder held on the relation, sorted.
	Modes []string
	// RelationSchema is the schema of the locked relation.
	RelationSchema string
	// RelationName is the name of the locked relation: the table itself or,
	// for a partitioned table, the table or one of its partitions at any
	// level.
	RelationName string
	// RelationOID is the locked relation's OID.
	RelationOID uint32
}

// TableUsage is the evidence of how recently a table was used, read from
// the cumulative statistics of the one server the pool is connected to. It
// is only built by ReadTableUsage, and it describes that server alone:
// reads served by any other server (a streaming replica, an Aurora reader)
// are not in it. Judge turns it into a UsageVerdict for a window.
//
// A partitioned table stores no rows and the server keeps its scan
// statistics on the leaf partitions, so its evidence is the evidence of
// every leaf partition, at every level, and the counters and scan times
// report the leaves taken together.
type TableUsage struct {
	schema           string
	table            string
	oid              uint32
	partitioned      bool
	serverVersionNum int
	inRecovery       bool
	trackCounts      bool
	readAt           time.Time
	statsReset       *time.Time
	// leaves are the relations that hold the table's rows and statistics:
	// the table itself when it is an ordinary table, its leaf partitions
	// when it is partitioned. Sorted by schema, then name.
	leaves      []PartitionUsage
	lockHolders []LockHolder
}

// Schema returns the schema qualification the read ran with (empty when the
// lookup used the session search_path).
func (u TableUsage) Schema() string { return u.schema }

// Table returns the table name the read ran with.
func (u TableUsage) Table() string { return u.table }

// OID returns the table's relation OID. The statistics are keyed on it, so
// a table recreated or swapped under the same name starts a new history.
func (u TableUsage) OID() uint32 { return u.oid }

// Partitioned reports whether the table is a partitioned table, whose
// evidence is read from its leaf partitions.
func (u TableUsage) Partitioned() bool { return u.partitioned }

// Partitions returns the evidence read for each leaf partition of a
// partitioned table, at every level, the default partition included,
// sorted by schema then name. It is empty for an ordinary table and for a
// partitioned table with no leaf partition.
func (u TableUsage) Partitions() []PartitionUsage {
	if !u.partitioned {
		return nil
	}
	return slices.Clone(u.leaves)
}

// ServerVersionNum returns the server's server_version_num.
func (u TableUsage) ServerVersionNum() int { return u.serverVersionNum }

// ScanTimesRecorded reports whether the server records when a table was
// last scanned: PostgreSQL 16 and later do, 14 and 15 do not.
func (u TableUsage) ScanTimesRecorded() bool {
	return u.serverVersionNum >= scanTimesSinceVersionNum
}

// InRecovery reports whether the server was a standby when read: its
// statistics then describe what that standby served, not the primary.
func (u TableUsage) InRecovery() bool { return u.inRecovery }

// TrackCounts reports whether track_counts was on for the reading session.
// With it off the server collects no table statistics at all.
func (u TableUsage) TrackCounts() bool { return u.trackCounts }

// ReadAt returns the server's now() for the read's transaction: every
// window is measured back from this, never from the client's clock.
func (u TableUsage) ReadAt() time.Time { return u.readAt }

// StatsReset returns when the current database's statistics were last
// reset (pg_stat_database.stats_reset). ok is false when the server reports
// none: no reset since the statistics began, or statistics discarded by
// crash recovery — either way the time counting began is unknown.
func (u TableUsage) StatsReset() (at time.Time, ok bool) { return derefTime(u.statsReset) }

// LastSeqScan returns when the server last recorded a sequential scan of
// the table — for a partitioned table, the latest across its leaf
// partitions. ok is false when it recorded none since its statistics
// began, and always on a server that does not record scan times.
func (u TableUsage) LastSeqScan() (at time.Time, ok bool) {
	return derefTime(latestScan(u.leaves, PartitionUsage.seqScanTime))
}

// LastIdxScan returns when the server last recorded an index scan on any of
// the table's indexes — for a partitioned table, the latest across its leaf
// partitions' indexes. ok is false when it recorded none since their
// statistics began, when there is no index, and always on a server that
// does not record scan times.
func (u TableUsage) LastIdxScan() (at time.Time, ok bool) {
	return derefTime(latestScan(u.leaves, PartitionUsage.idxScanTime))
}

// SeqScans returns the cumulative count of sequential scans of the table,
// summed over the leaf partitions of a partitioned table.
func (u TableUsage) SeqScans() int64 { return sumLeaves(u.leaves, PartitionUsage.SeqScans) }

// IdxScans returns the cumulative count of index scans on the table's
// indexes (0 for a table with no index), summed over the leaf partitions
// of a partitioned table.
func (u TableUsage) IdxScans() int64 { return sumLeaves(u.leaves, PartitionUsage.IdxScans) }

// TuplesInserted returns the cumulative count of rows inserted, summed over
// the leaf partitions of a partitioned table. Write counters carry no time,
// so Judge never decides on them.
func (u TableUsage) TuplesInserted() int64 {
	return sumLeaves(u.leaves, func(p PartitionUsage) int64 { return p.tuplesInserted })
}

// TuplesUpdated returns the cumulative count of rows updated, summed over
// the leaf partitions of a partitioned table. Write counters carry no time,
// so Judge never decides on them.
func (u TableUsage) TuplesUpdated() int64 {
	return sumLeaves(u.leaves, func(p PartitionUsage) int64 { return p.tuplesUpdated })
}

// TuplesDeleted returns the cumulative count of rows deleted, summed over
// the leaf partitions of a partitioned table. Write counters carry no time,
// so Judge never decides on them.
func (u TableUsage) TuplesDeleted() int64 {
	return sumLeaves(u.leaves, func(p PartitionUsage) int64 { return p.tuplesDeleted })
}

// LockHolders returns the sessions, other than the reading one, that held a
// granted lock on the table, or on any of a partitioned table's partitions,
// when it was read: one entry per session and relation.
func (u TableUsage) LockHolders() []LockHolder { return slices.Clone(u.lockHolders) }

func derefTime(t *time.Time) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	return *t, true
}

// ReadTableUsage reads how recently schema.table (search_path when schema
// is empty) was used on the server the pool is connected to, in one
// read-only transaction bounded by its own lock and statement timeouts. It
// reads the server's version, whether it is in recovery, the reading
// session's track_counts, the server's now(), the database's statistics
// reset time, the scan and write counters (and, on PostgreSQL 16 and later,
// the last scan times) of the table or, when it is partitioned, of every
// leaf partition at every level, and the sessions other than its own
// holding a lock on the table or any of its partitions. It takes no lock on
// any of them and changes nothing.
//
// Declarative partitions are followed; inheritance children of an ordinary
// table are not: they are tables of their own, and a read through the
// parent is recorded on the parent too.
//
// A missing name is ErrTableNotFound, and a relation that is neither an
// ordinary nor a partitioned table is ErrNotTable.
func ReadTableUsage(ctx context.Context, pool *pgxpool.Pool, schema, table string) (TableUsage, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return TableUsage{}, fmt.Errorf("begin table usage read %s: %w", qualifiedName(schema, table), err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	// INV: LK-2 — the read bounds itself rather than trusting the pool's
	// session defaults.
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(usageReadLockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(usageReadStatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return TableUsage{}, fmt.Errorf("set table usage read budgets: %w", err)
	}
	u := TableUsage{schema: schema, table: table}
	if err := readUsageServerFacts(ctx, tx, &u); err != nil {
		return TableUsage{}, err
	}
	tree, err := readUsageRelations(ctx, tx, &u)
	if err != nil {
		return TableUsage{}, err
	}
	if err := readUsageLockHolders(ctx, tx, &u, tree); err != nil {
		return TableUsage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TableUsage{}, fmt.Errorf("commit table usage read %s: %w", qualifiedName(schema, table), err)
	}
	return u, nil
}

// readUsageServerFacts resolves the table and reads the server-wide facts
// the verdict depends on.
func readUsageServerFacts(ctx context.Context, tx pgx.Tx, u *TableUsage) error {
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so no lookalike on the session's search_path
	// can report a primary for a standby, another clock, or another reset.
	const q = `
		SELECT c.oid, c.relkind::pg_catalog.text,
		       pg_catalog.current_setting('server_version_num')::pg_catalog.int4,
		       pg_catalog.pg_is_in_recovery(),
		       pg_catalog.current_setting('track_counts')::pg_catalog.bool,
		       pg_catalog.now(),
		       (SELECT d.stats_reset FROM pg_catalog.pg_stat_database d
		         WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database())
		FROM pg_catalog.pg_class c
		WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(
			CASE WHEN $1 OPERATOR(pg_catalog.=) '' THEN pg_catalog.quote_ident($2)
			     ELSE pg_catalog.quote_ident($1) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident($2) END)`
	var relkind string
	err := tx.QueryRow(ctx, q, u.schema, u.table).Scan(
		&u.oid, &relkind, &u.serverVersionNum, &u.inRecovery, &u.trackCounts, &u.readAt, &u.statsReset)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrTableNotFound, qualifiedName(u.schema, u.table))
	}
	if err != nil {
		return fmt.Errorf("read table usage server facts %s: %w", qualifiedName(u.schema, u.table), err)
	}
	switch relkind {
	case relkindOrdinary:
		u.partitioned = false
	case relkindPartitioned:
		u.partitioned = true
	default:
		return fmt.Errorf("%w: %s has relkind %q", ErrNotTable, qualifiedName(u.schema, u.table), relkind)
	}
	return nil
}

// usageRelationsQuery reads the table and, when it is partitioned, every
// partition below it at every level, with each relation's counters, on a
// server that records no scan times; the two NULLs stand where PostgreSQL
// 16 reports them. usageRelationsWithScanTimesQuery is the same read on
// PostgreSQL 16 and later.
//
// The tree is walked through pg_inherits rather than with
// pg_partition_tree(), which takes ACCESS SHARE on every partition; a
// catalog read takes no lock on the relations it describes. Only children
// that are partitions are followed, so an ordinary table's inheritance
// children are not. A foreign-table partition has no row in
// pg_stat_all_tables, so the statistics are an outer join: its seq_scan
// reads NULL, which marks the missing row, and its other counters read 0.
// idx_scan is NULL for a table with no index and reads 0.
//
// INV: CO-9 — every relation, operator, and type is pg_catalog-qualified.
const (
	usageRelationsTree = `
		WITH RECURSIVE tree(relid) AS (
			SELECT $1::pg_catalog.oid
			UNION ALL
			SELECT i.inhrelid
			FROM tree t
			JOIN pg_catalog.pg_inherits i ON i.inhparent OPERATOR(pg_catalog.=) t.relid
			JOIN pg_catalog.pg_class child ON child.oid OPERATOR(pg_catalog.=) i.inhrelid
			WHERE child.relispartition
		)`
	usageRelationsFrom = `
		FROM tree t
		JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) t.relid
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid OPERATOR(pg_catalog.=) c.oid`
	usageRelationsQuery = usageRelationsTree + `
		SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text, c.relkind::pg_catalog.text,
		       s.seq_scan, COALESCE(s.idx_scan, 0),
		       COALESCE(s.n_tup_ins, 0), COALESCE(s.n_tup_upd, 0), COALESCE(s.n_tup_del, 0),
		       NULL::pg_catalog.timestamptz, NULL::pg_catalog.timestamptz` + usageRelationsFrom
	usageRelationsWithScanTimesQuery = usageRelationsTree + `
		SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text, c.relkind::pg_catalog.text,
		       s.seq_scan, COALESCE(s.idx_scan, 0),
		       COALESCE(s.n_tup_ins, 0), COALESCE(s.n_tup_upd, 0), COALESCE(s.n_tup_del, 0),
		       s.last_seq_scan, s.last_idx_scan` + usageRelationsFrom
)

// usageTreeRelation is one relation of the table's tree as the relations
// read returns it.
type usageTreeRelation struct {
	usage   PartitionUsage
	relkind string
	// seqScan is NULL exactly when the relation has no statistics row.
	seqScan *int64
}

// readUsageRelations reads the table's tree and records its leaves on u:
// the table itself when it is ordinary, otherwise every leaf partition. It
// returns every relation of the tree, the partitioned ones included, for
// the lock read.
func readUsageRelations(ctx context.Context, tx pgx.Tx, u *TableUsage) ([]PartitionUsage, error) {
	q := usageRelationsQuery
	if u.ScanTimesRecorded() {
		q = usageRelationsWithScanTimesQuery
	}
	rows, err := tx.Query(ctx, q, u.oid)
	if err != nil {
		return nil, fmt.Errorf("read table usage counters %s: %w", qualifiedName(u.schema, u.table), err)
	}
	relations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (usageTreeRelation, error) {
		var r usageTreeRelation
		p := &r.usage
		err := row.Scan(&p.oid, &p.schema, &p.name, &r.relkind, &r.seqScan, &p.idxScans,
			&p.tuplesInserted, &p.tuplesUpdated, &p.tuplesDeleted, &p.lastSeqScan, &p.lastIdxScan)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read table usage counters %s: %w", qualifiedName(u.schema, u.table), err)
	}
	tree := make([]PartitionUsage, 0, len(relations))
	for i := range relations {
		r := &relations[i]
		leaf, err := usageLeaf(r)
		if err != nil {
			return nil, fmt.Errorf("read table usage counters %s: %w", qualifiedName(u.schema, u.table), err)
		}
		tree = append(tree, r.usage)
		if leaf {
			u.leaves = append(u.leaves, r.usage)
		}
	}
	slices.SortFunc(u.leaves, comparePartitions)
	return tree, nil
}

// usageLeaf reports whether a relation of the tree holds rows and so
// statistics, and completes its evidence: an ordinary table always has a
// statistics row, a foreign-table partition never does, and a partitioned
// relation — the table itself or a sub-partitioned partition — holds no
// rows of its own.
func usageLeaf(r *usageTreeRelation) (bool, error) {
	switch r.relkind {
	case relkindOrdinary:
		if r.seqScan == nil {
			return false, fmt.Errorf("%w: ordinary table %s has no statistics row",
				dbconn.ErrInvariantViolation, qualifiedName(r.usage.schema, r.usage.name))
		}
		r.usage.seqScans = *r.seqScan
		return true, nil
	case relkindForeign:
		r.usage.foreign = true
		return true, nil
	case relkindPartitioned:
		return false, nil
	default:
		return false, fmt.Errorf("%w: partition %s has relkind %q",
			dbconn.ErrInvariantViolation, qualifiedName(r.usage.schema, r.usage.name), r.relkind)
	}
}

// readUsageLockHolders reads every session other than the reading one that
// holds a granted lock in this database on any relation of the table's
// tree, one row per holder and relation. A prepared transaction holds its
// locks with no backend, so the activity join is an outer join and its
// holder reads with no PID.
func readUsageLockHolders(ctx context.Context, tx pgx.Tx, u *TableUsage, tree []PartitionUsage) error {
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a lookalike pg_locks cannot hide a holder.
	const q = `
		SELECT l.pid, COALESCE(a.application_name, ''), l.relation, pg_catalog.array_agg(l.mode)
		FROM pg_catalog.pg_locks l
		LEFT JOIN pg_catalog.pg_stat_activity a ON a.pid OPERATOR(pg_catalog.=) l.pid
		WHERE l.locktype OPERATOR(pg_catalog.=) 'relation'
		  AND l.relation OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.oid[])
		  AND l.database OPERATOR(pg_catalog.=) (SELECT d.oid FROM pg_catalog.pg_database d
		                                          WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database())
		  AND l.granted
		  AND (l.pid IS NULL OR l.pid OPERATOR(pg_catalog.<>) pg_catalog.pg_backend_pid())
		GROUP BY l.virtualtransaction, l.pid, a.application_name, l.relation`
	names := make(map[uint32]PartitionUsage, len(tree))
	oids := make([]uint32, 0, len(tree))
	for _, r := range tree {
		names[r.oid] = r
		oids = append(oids, r.oid)
	}
	rows, err := tx.Query(ctx, q, oids)
	if err != nil {
		return fmt.Errorf("read table usage lock holders %s: %w", qualifiedName(u.schema, u.table), err)
	}
	holders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LockHolder, error) {
		var pid *int32
		var h LockHolder
		if err := row.Scan(&pid, &h.ApplicationName, &h.RelationOID, &h.Modes); err != nil {
			return LockHolder{}, err
		}
		if pid != nil {
			h.PID = *pid
		}
		relation := names[h.RelationOID]
		h.RelationSchema, h.RelationName = relation.schema, relation.name
		slices.Sort(h.Modes)
		return h, nil
	})
	if err != nil {
		return fmt.Errorf("read table usage lock holders %s: %w", qualifiedName(u.schema, u.table), err)
	}
	slices.SortFunc(holders, func(a, b LockHolder) int {
		return cmp.Or(cmp.Compare(a.PID, b.PID),
			cmp.Compare(a.RelationSchema, b.RelationSchema), cmp.Compare(a.RelationName, b.RelationName))
	})
	u.lockHolders = holders
	return nil
}
