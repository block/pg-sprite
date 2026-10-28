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
	// IdxScans is the cumulative count of scans of the index since
	// StatsReset (pg_stat_all_indexes.idx_scan).
	IdxScans int64
	// LastIdxScan is when the server last recorded a scan of the index; nil
	// when it recorded none since its statistics began, and always on
	// PostgreSQL 14 and 15, which do not record scan times.
	LastIdxScan *time.Time
	// StatsReset is when the database's statistics were last reset; nil
	// when the server reports none, so the start of counting is unknown.
	StatsReset *time.Time
	// ReadAt is the server's now() for the read.
	ReadAt time.Time
	// Unique reports whether the index enforces uniqueness.
	Unique bool
	// TrackCounts is the reading session's track_counts; with it off the
	// server counts no scans at all.
	TrackCounts bool
}

// The index usage read: the index's scan count and uniqueness, the
// database's statistics reset, the server's clock, and track_counts, from
// one statistics snapshot. indexUsageWithScanTimeQuery adds last_idx_scan,
// which PostgreSQL 16 introduced. Neither reads the relation itself, so
// neither takes a lock on the index or its table.
//
// INV: CO-9 — every function, operator, relation, and type is
// pg_catalog-qualified.
const (
	indexUsageHead = `
		SELECT COALESCE(s.idx_scan, 0), `
	indexUsageTail = `,
		       (SELECT d.stats_reset FROM pg_catalog.pg_stat_database d
		         WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()),
		       pg_catalog.now(), i.indisunique,
		       pg_catalog.current_setting('track_counts')::pg_catalog.bool
		FROM pg_catalog.pg_index i
		LEFT JOIN pg_catalog.pg_stat_all_indexes s ON s.indexrelid OPERATOR(pg_catalog.=) i.indexrelid
		WHERE i.indexrelid OPERATOR(pg_catalog.=) $1::pg_catalog.oid`
	indexUsageQuery             = indexUsageHead + "NULL::pg_catalog.timestamptz" + indexUsageTail
	indexUsageWithScanTimeQuery = indexUsageHead + "s.last_idx_scan" + indexUsageTail
)

// errIndexUsageIndexNotPresent means the index the caller resolved was
// dropped before its usage was read.
var errIndexUsageIndexNotPresent = errors.New("the index no longer exists")

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
	err = tx.QueryRow(ctx, q, indexOID).Scan(&u.IdxScans, &u.LastIdxScan, &u.StatsReset, &u.ReadAt, &u.Unique, &u.TrackCounts)
	if errors.Is(err, pgx.ErrNoRows) {
		return IndexUsage{}, fmt.Errorf("read index usage %d: %w", indexOID, errIndexUsageIndexNotPresent)
	}
	if err != nil {
		return IndexUsage{}, fmt.Errorf("read index usage %d: %w", indexOID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IndexUsage{}, fmt.Errorf("commit index usage read %d: %w", indexOID, err)
	}
	return u, nil
}
