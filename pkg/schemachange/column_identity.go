package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// columnPosition is a live column's name and attribute number.
type columnPosition struct {
	name   string
	attnum int
}

// readColumnPositions lists a relation's live columns in attribute order.
func readColumnPositions(ctx context.Context, tx pgx.Tx, oid uint32) ([]columnPosition, error) {
	// INV: CO-9
	rows, err := tx.Query(ctx, `
		SELECT a.attname, a.attnum::int
		FROM pg_catalog.pg_attribute a
		WHERE a.attrelid OPERATOR(pg_catalog.=) $1 AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, fmt.Errorf("read column positions: %w", err)
	}
	positions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (columnPosition, error) {
		var p columnPosition
		err := row.Scan(&p.name, &p.attnum)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("read column positions: %w", err)
	}
	return positions, nil
}

// sameColumns names the source columns that are still the same column on the
// shadow, not merely a column of the same name. CREATE TABLE … LIKE numbers
// the shadow's columns 1..n in the source's live column order, and the gated
// statement never renumbers a column it keeps — a retype keeps its number —
// while an added column takes a number past n. So a source column at live
// position i is the same column on the shadow exactly when the shadow's
// column of that name has attribute number i; a column the statement dropped
// and added again under one name is a new column with a number past n.
//
// Both inputs are in attribute order. The source's numbers may have gaps
// from columns dropped before the build, so its position is its index, not
// its attribute number.
func sameColumns(source, shadow []columnPosition) map[string]bool {
	shadowNumber := make(map[string]int, len(shadow))
	for _, c := range shadow {
		shadowNumber[c.name] = c.attnum
	}
	same := make(map[string]bool, len(source))
	for i, c := range source {
		if number, ok := shadowNumber[c.name]; ok && number == i+1 {
			same[c.name] = true
		}
	}
	return same
}
