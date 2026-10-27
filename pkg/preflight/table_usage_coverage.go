package preflight

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// statsFetchConsistencySinceVersionNum is the first server_version_num
// with the stats_fetch_consistency setting (PostgreSQL 15).
const statsFetchConsistencySinceVersionNum = 150000

// pinUsageStatsSnapshot makes every statistics read of the transaction come
// from one snapshot of the database's statistics, taken at its first read.
// PostgreSQL 15 and later otherwise default to stats_fetch_consistency =
// cache, which fetches each object's statistics when it is first read, so
// a pg_stat_reset() between the reset-time read and the table read could
// pair an old reset time with scan times the reset already cleared.
// PostgreSQL 14 has no such setting and needs none: its backend reads the
// collector's statistics once per transaction. It must run before the
// transaction's first statistics read.
func pinUsageStatsSnapshot(ctx context.Context, tx pgx.Tx) error {
	var versionNum int
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4").Scan(&versionNum); err != nil {
		return fmt.Errorf("read server version for the table usage read: %w", err)
	}
	if versionNum < statsFetchConsistencySinceVersionNum {
		return nil
	}
	if _, err := tx.Exec(ctx, "SET LOCAL stats_fetch_consistency = 'snapshot'"); err != nil {
		return fmt.Errorf("pin the table usage statistics snapshot: %w", err)
	}
	return nil
}

// readUsageTrackCountsSettings records on u whether a saved per-role or
// per-database setting turns track_counts off for some role or for this
// database. The reading session's own value already reflects the server's
// configuration and the reading role's settings; this catches the
// settings of every other role. Each saved value was validated as a
// boolean when it was set, so the cast cannot fail.
func readUsageTrackCountsSettings(ctx context.Context, tx pgx.Tx, u *TableUsage) error {
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified.
	const q = `
		SELECT pg_catalog.split_part(setting.entry, '=', 2)::pg_catalog.bool
		FROM (SELECT pg_catalog.unnest(s.setconfig) AS entry
		      FROM pg_catalog.pg_db_role_setting s
		      WHERE s.setdatabase OPERATOR(pg_catalog.=) 0
		         OR s.setdatabase OPERATOR(pg_catalog.=) (SELECT d.oid FROM pg_catalog.pg_database d
		                                                   WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database())
		     ) setting
		WHERE pg_catalog.split_part(setting.entry, '=', 1) OPERATOR(pg_catalog.=) 'track_counts'`
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("read track_counts settings: %w", err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	if err != nil {
		return fmt.Errorf("read track_counts settings: %w", err)
	}
	u.trackCountsOffBySetting = slices.Contains(values, false)
	return nil
}
