package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrTimeZoneDrift reports a change between timestamp and timestamptz that
// would run in a session whose time zone is not UTC. The planner routes
// that change native only when it read UTC, where PostgreSQL keeps every
// stored value; in any other zone the change rewrites the table under its
// exclusive lock and moves every value by the zone's offset, which the
// reviewed plan never named.
var ErrTimeZoneDrift = errors.New("the session time zone is not the UTC zone a native change between timestamp and timestamptz was planned in")

// timeZoneSwapCandidates returns the operations of st that change a column
// to scalar timestamp or timestamptz with no USING expression: the ones
// that read every stored value in the session time zone when the column
// is the other of the two. The live type decides which of them swap, read
// inside the attempt.
func timeZoneSwapCandidates(st statement.Statement) ([]statement.Op, error) {
	if st.Kind() != statement.KindAlterTable {
		return nil, nil
	}
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return nil, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	var candidates []statement.Op
	for _, op := range ops {
		if op.Kind != statement.OpAlterColumnType || op.HasUsing || op.NewTypeArray {
			continue
		}
		if op.NewTypeSchema != "" && op.NewTypeSchema != "pg_catalog" {
			continue
		}
		if op.NewType == "timestamp" || op.NewType == "timestamptz" {
			candidates = append(candidates, op)
		}
	}
	return candidates, nil
}

// refuseTimeZoneSwapOutsideUTC reads the time zone the attempt's own
// transaction runs in, which is the zone the statement converts in, and
// refuses with ErrTimeZoneDrift when it is not UTC and one of candidates
// changes a column between timestamp and timestamptz. A zone setting that
// changed between planning and execution, through ALTER DATABASE, ALTER
// ROLE, or a configuration reload, then stops the change before it locks
// the table.
func refuseTimeZoneSwapOutsideUTC(ctx context.Context, tx pgx.Tx, st statement.Statement, candidates []statement.Op) error {
	var zone string
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('TimeZone')").Scan(&zone); err != nil {
		return fmt.Errorf("read the session time zone for %q: %w", st.SQL(), err)
	}
	if zone == "UTC" || zone == "Etc/UTC" {
		return nil
	}
	// INV: CO-9 — the table is resolved by its quoted, schema-qualified
	// name and every catalog name is pg_catalog-qualified.
	const q = `
		SELECT t.typname
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_type t ON t.oid OPERATOR(pg_catalog.=) a.atttypid
		WHERE a.attrelid OPERATOR(pg_catalog.=) $1::pg_catalog.regclass
		  AND a.attname OPERATOR(pg_catalog.=) $2
		  AND NOT a.attisdropped`
	table := pgx.Identifier{st.Schema(), st.Table()}.Sanitize()
	for _, op := range candidates {
		var live string
		err := tx.QueryRow(ctx, q, table, op.Column).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) {
			// No such column: the statement itself fails on it, with the
			// server's own error, and nothing is converted.
			continue
		}
		if err != nil {
			return fmt.Errorf("read the type of column %q of %s: %w", op.Column, qualifiedName(st.Schema(), st.Table()), err)
		}
		if (live == "timestamp" && op.NewType == "timestamptz") || (live == "timestamptz" && op.NewType == "timestamp") {
			return fmt.Errorf("%w: %q changes column %q from %s to %s, and this session reads its values in %q; plan again",
				ErrTimeZoneDrift, st.SQL(), op.Column, live, op.NewType, zone)
		}
	}
	return nil
}
