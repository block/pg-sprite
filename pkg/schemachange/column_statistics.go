package schemachange

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// ColumnStatisticsTarget is one column's explicit ALTER COLUMN … SET
// STATISTICS target. CREATE TABLE … LIKE INCLUDING ALL leaves every shadow
// column at the server default, so the builder re-applies each explicit
// target; a column at the default is not recorded.
type ColumnStatisticsTarget struct {
	// Column is the column name.
	Column string `json:"column"`
	// Target is the per-column statistics target.
	Target int `json:"target"`
}

// readColumnStatisticsTargets lists the columns whose statistics target is
// set explicitly. The server stores the default as -1, or as NULL on
// servers whose catalog made the column nullable; both read as "default"
// and are left out.
func readColumnStatisticsTargets(ctx context.Context, tx pgx.Tx, oid uint32) ([]ColumnStatisticsTarget, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.attname, a.attstattarget::int
		FROM pg_attribute a
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
		  AND COALESCE(a.attstattarget, -1) <> -1
		ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, fmt.Errorf("read column statistics targets: %w", err)
	}
	targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ColumnStatisticsTarget, error) {
		var t ColumnStatisticsTarget
		err := row.Scan(&t.Column, &t.Target)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("read column statistics targets: %w", err)
	}
	return targets, nil
}

// applyColumnStatisticsTargets sets each explicit target on the shadow. The
// target is an integer the server reported, rendered as such; the column
// name goes through Sanitize.
func applyColumnStatisticsTargets(ctx context.Context, tx pgx.Tx, table string, targets []ColumnStatisticsTarget) error {
	for _, t := range targets {
		sql := "ALTER TABLE " + table + " ALTER COLUMN " + pgx.Identifier{t.Column}.Sanitize() +
			" SET STATISTICS " + strconv.Itoa(t.Target)
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("set statistics target of shadow column %s: %w", t.Column, err)
		}
	}
	return nil
}
