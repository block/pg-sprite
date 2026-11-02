package preflight

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IndexUsage is the evidence of how an index has been used, read from the
// cumulative statistics of the one server the pool is connected to. Like
// TableUsage it describes that server alone: scans served by a streaming
// replica or an Aurora reader are counted there, not here. It is built only
// by ReadIndexUsage.
type IndexUsage struct {
	// IdxScans is the cumulative count of scans of the index
	// (pg_stat_all_indexes.idx_scan). For a partitioned index it is the sum
	// over its leaf indexes, where the server records the scans; the
	// partitioned index itself has no statistics.
	IdxScans int64
	// LastIdxScan is when the server last recorded a scan of the index, or
	// of any of a partitioned index's leaf indexes; nil when it recorded
	// none since its statistics began, and always on PostgreSQL 14 and 15,
	// which do not record scan times.
	LastIdxScan *time.Time
	// StatsReset is when the database's statistics were last reset; nil
	// when the server reports none. An index created later has counted only
	// since its creation, which the server does not record.
	StatsReset *time.Time
	// ReadAt is the server's now() for the read.
	ReadAt time.Time
	// Unique reports whether the index enforces uniqueness.
	Unique bool
	// Partitioned reports whether the index is a partitioned index, whose
	// scans are counted on its leaf indexes.
	Partitioned bool
	// Partitions is the number of leaf indexes of a partitioned index, at
	// any depth; 0 for an index that is not partitioned.
	Partitions int
	// TrackCounts is the reading session's track_counts; with it off the
	// server counts no scans at all.
	TrackCounts bool
	// TrackCountsOffBySetting reports that a saved ALTER ROLE or ALTER
	// DATABASE setting turns track_counts off for some role or for this
	// database, so the scans of those sessions are not counted even when
	// the reading session's TrackCounts is on.
	TrackCountsOffBySetting bool
}

// The index usage read: the scan counts of the index's leaf indexes (the
// index itself unless it is partitioned), its uniqueness and relkind, the
// database's statistics reset, the server's clock, and track_counts, from
// one statistics snapshot. indexUsageWithScanTimeQuery adds last_idx_scan,
// which PostgreSQL 16 introduced. The tree walks pg_inherits rather than
// calling pg_partition_tree, which locks every partition. Nothing here
// reads a relation itself, so the read takes no lock on the index or its
// table. statsRows counts the leaves the statistics view lists, so a
// missing row is told apart from a zero count.
//
// INV: CO-9 — every function, operator, relation, and type is
// pg_catalog-qualified.
const (
	indexUsageHead = `
		WITH RECURSIVE tree(relid) AS (
			SELECT $1::pg_catalog.oid
			UNION ALL
			SELECT h.inhrelid
			FROM tree t
			JOIN pg_catalog.pg_inherits h ON h.inhparent OPERATOR(pg_catalog.=) t.relid
			JOIN pg_catalog.pg_class child ON child.oid OPERATOR(pg_catalog.=) h.inhrelid
			WHERE child.relispartition
		),
		leaves AS (
			SELECT s.indexrelid AS stats_row, s.idx_scan, `
	indexUsageTail = ` AS last_idx_scan
			FROM tree t
			JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) t.relid
			LEFT JOIN pg_catalog.pg_stat_all_indexes s ON s.indexrelid OPERATOR(pg_catalog.=) t.relid
			WHERE c.relkind OPERATOR(pg_catalog.=) 'i'
		)
		SELECT (SELECT pg_catalog.count(*) FROM leaves),
		       (SELECT pg_catalog.count(l.stats_row) FROM leaves l),
		       (SELECT COALESCE(pg_catalog.sum(l.idx_scan), 0)::pg_catalog.int8 FROM leaves l),
		       (SELECT pg_catalog.max(l.last_idx_scan) FROM leaves l),
		       (SELECT d.stats_reset FROM pg_catalog.pg_stat_database d
		         WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()),
		       pg_catalog.now(), i.indisunique,
		       ic.relkind OPERATOR(pg_catalog.=) 'I',
		       pg_catalog.current_setting('track_counts')::pg_catalog.bool
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class ic ON ic.oid OPERATOR(pg_catalog.=) i.indexrelid
		WHERE i.indexrelid OPERATOR(pg_catalog.=) $1::pg_catalog.oid`
	indexUsageQuery             = indexUsageHead + "NULL::pg_catalog.timestamptz" + indexUsageTail
	indexUsageWithScanTimeQuery = indexUsageHead + "s.last_idx_scan" + indexUsageTail
)

// errIndexUsageIndexNotPresent means the index the caller resolved was
// dropped before its usage was read.
var errIndexUsageIndexNotPresent = errors.New("the index no longer exists")

// errIndexUsageStatisticsMissing means the statistics view lists no row for
// a leaf index, so its scans are unknown rather than zero.
var errIndexUsageStatisticsMissing = errors.New("the server reports no statistics for the index")

// ReadIndexUsage reads the usage evidence of the index with the given OID
// on the server the pool is connected to, in one read-only transaction
// bounded by its own lock and statement timeouts, with every statistic from
// one snapshot. Keying on the OID reads the index the caller resolved, not
// whatever holds its name now. It changes nothing.
func ReadIndexUsage(ctx context.Context, pool *pgxpool.Pool, indexOID uint32) (IndexUsage, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return IndexUsage{}, fmt.Errorf("begin index usage read %d: %w", indexOID, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	// INV: LK-2 — the read bounds itself rather than trusting the pool's
	// session defaults.
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(usageReadLockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(usageReadStatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return IndexUsage{}, fmt.Errorf("set index usage read budgets: %w", err)
	}
	if err := pinUsageStatsSnapshot(ctx, tx); err != nil {
		return IndexUsage{}, err
	}
	var versionNum int
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4").Scan(&versionNum); err != nil {
		return IndexUsage{}, fmt.Errorf("read server version for the index usage read: %w", err)
	}
	q := indexUsageQuery
	if versionNum >= scanTimesSinceVersionNum {
		q = indexUsageWithScanTimeQuery
	}
	var u IndexUsage
	var leaves, statsRows int
	err = tx.QueryRow(ctx, q, indexOID).Scan(&leaves, &statsRows, &u.IdxScans, &u.LastIdxScan,
		&u.StatsReset, &u.ReadAt, &u.Unique, &u.Partitioned, &u.TrackCounts)
	if errors.Is(err, pgx.ErrNoRows) {
		return IndexUsage{}, fmt.Errorf("read index usage %d: %w", indexOID, errIndexUsageIndexNotPresent)
	}
	if err != nil {
		return IndexUsage{}, fmt.Errorf("read index usage %d: %w", indexOID, err)
	}
	if statsRows != leaves {
		return IndexUsage{}, fmt.Errorf("read index usage %d: %w (%d of %d leaf indexes listed)",
			indexOID, errIndexUsageStatisticsMissing, statsRows, leaves)
	}
	if u.Partitioned {
		u.Partitions = leaves
	}
	if u.TrackCountsOffBySetting, err = readUsageTrackCountsOffBySetting(ctx, tx); err != nil {
		return IndexUsage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IndexUsage{}, fmt.Errorf("commit index usage read %d: %w", indexOID, err)
	}
	return u, nil
}
