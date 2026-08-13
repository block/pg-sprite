// Package preflight verifies preconditions before the engine writes anything
// (invariant ST-6). In Phase 1 that is the table-size guard in front of the
// optimistic attempt: a cancelled rewrite attempt is not a free probe — it
// holds ACCESS EXCLUSIVE and does real rewrite work for the full statement
// budget — so above a size threshold the attempt is skipped entirely.
//
// This is a safety-critical core package: see SAFETY.md. It returns proof
// types with package-private constructors; dangerous downstream APIs accept
// only the proof.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NoSizeLimit is a size limit no PostgreSQL relation can exceed. Callers
// pass it when the check should prove only existence and kind — the online
// sequence path, whose long steps are safe on any size by design (the size
// guard protects blind attempts, not planner-proven online idioms).
const NoSizeLimit int64 = math.MaxInt64

// ErrTableNotFound is returned when the target table does not exist (or is
// not visible with the session's search_path).
var ErrTableNotFound = errors.New("table not found")

// ErrNotTable is returned when the target exists but is not an ordinary or
// partitioned table (e.g. a view or foreign table).
var ErrNotTable = errors.New("not an ordinary or partitioned table")

// SizeError reports that the table exceeds the configured size threshold, so
// the optimistic attempt must be skipped. It is a refusal input, not an
// operational failure.
type SizeError struct {
	// TotalBytes is the table's measured on-disk size (all partitions,
	// including indexes and TOAST).
	TotalBytes int64
	// LimitBytes is the threshold that was exceeded.
	LimitBytes int64
}

// Error implements the error interface.
func (e *SizeError) Error() string {
	return fmt.Sprintf("table size %d bytes exceeds the %d-byte threshold for an optimistic attempt", e.TotalBytes, e.LimitBytes)
}

// PreflightedTable proves the target table exists, is a table, and is under
// the size threshold for an optimistic attempt. It can only be constructed by
// CheckTable in this package.
type PreflightedTable struct {
	schema     string
	table      string
	totalBytes int64
	relTuples  float64
}

// Schema returns the schema qualification the check ran with (empty when the
// lookup used the session search_path).
func (t PreflightedTable) Schema() string { return t.schema }

// Table returns the verified table name.
func (t PreflightedTable) Table() string { return t.table }

// TotalBytes returns the measured on-disk size across all partitions,
// including indexes and TOAST.
func (t PreflightedTable) TotalBytes() int64 { return t.totalBytes }

// RelTuples returns the planner's row estimate (-1 when the table has never
// been vacuumed or analyzed). Reporting only — the size guard's authority is
// bytes on disk.
func (t PreflightedTable) RelTuples() float64 { return t.relTuples }

// CheckTable verifies that schema.table (search_path when schema is empty)
// exists, is an ordinary or partitioned table, and is at most limitBytes on
// disk. Above the limit it returns a *SizeError; on success it returns the
// PreflightedTable proof.
func CheckTable(ctx context.Context, pool *pgxpool.Pool, schema, table string, limitBytes int64) (PreflightedTable, error) {
	if limitBytes <= 0 {
		return PreflightedTable{}, fmt.Errorf("size limit must be positive, got %d", limitBytes)
	}
	// INV: ST-6 — size facts are measured on-disk bytes
	// (pg_total_relation_size: heap, indexes, and TOAST — the rewrite the
	// guard fears rebuilds every index under the same ACCESS EXCLUSIVE
	// lock, so an index-heavy table must not sail under the threshold),
	// summed over pg_partition_tree so a partitioned parent (whose own
	// relation is 0 bytes) cannot fail open. Stale planner statistics
	// (relpages) are never the guard's authority.
	// The table's own size plus every descendant in its partition tree:
	// pg_partition_tree returns no rows for a plain table (its own
	// pg_total_relation_size carries the total) and the parent's own
	// relation is 0 bytes for a partitioned table (the descendants carry
	// the total).
	const q = `
		SELECT c.relkind::text,
		       pg_total_relation_size(c.oid) +
		       (SELECT COALESCE(sum(pg_total_relation_size(p.relid)), 0)
		          FROM pg_partition_tree(c.oid) p
		         WHERE p.relid <> c.oid),
		       c.reltuples
		FROM pg_class c
		WHERE c.oid = to_regclass(
			CASE WHEN $1 = '' THEN quote_ident($2)
			     ELSE quote_ident($1) || '.' || quote_ident($2) END)`
	var relkind string
	var totalBytes int64
	var relTuples float64
	err := pool.QueryRow(ctx, q, schema, table).Scan(&relkind, &totalBytes, &relTuples)
	if errors.Is(err, pgx.ErrNoRows) {
		return PreflightedTable{}, fmt.Errorf("%w: %s", ErrTableNotFound, qualifiedName(schema, table))
	}
	if err != nil {
		return PreflightedTable{}, fmt.Errorf("look up table %s: %w", qualifiedName(schema, table), err)
	}
	// relkind 'r' is an ordinary table, 'p' a partitioned parent; anything
	// else (view, matview, foreign table, sequence) is refused fail-closed.
	if relkind != "r" && relkind != "p" {
		return PreflightedTable{}, fmt.Errorf("%w: %s has relkind %q", ErrNotTable, qualifiedName(schema, table), relkind)
	}
	if totalBytes > limitBytes {
		return PreflightedTable{}, &SizeError{TotalBytes: totalBytes, LimitBytes: limitBytes}
	}
	return PreflightedTable{schema: schema, table: table, totalBytes: totalBytes, relTuples: relTuples}, nil
}

// qualifiedName renders schema.table for error messages, omitting the dot
// when the name is unqualified.
func qualifiedName(schema, table string) string {
	if schema == "" {
		return table
	}
	return schema + "." + table
}
