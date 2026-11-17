package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sideSQL is the two statements a pass runs on one side of a chunk: the
// digest, which every comparison reads, and the rows, which a live pass
// reads for a chunk it must settle key by key.
type sideSQL struct {
	digest string
	rows   string
}

// statements is one pass's frozen SQL for both sides. The source side
// carries the schema change's USING expressions, the shadow side none: the
// shadow already holds the converted values.
type statements struct {
	source sideSQL
	shadow sideSQL
}

// digestStatements freezes the pass's statements from the shadow's column
// types, read in a guarded transaction so the shadow whose types they name
// is the shadow the proof describes. The same transaction reads the
// source's row estimate the pass reports as rows_to_hash.
func (v *Verifier) digestStatements(ctx context.Context, pool *pgxpool.Pool) (statements, error) {
	tx, err := v.begin(ctx, pool, snapshotRead())
	if err != nil {
		return statements{}, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	types, err := shadowColumnTypes(ctx, tx, v.shadow)
	if err != nil {
		return statements{}, err
	}
	if err := v.countRowsToHash(ctx, tx); err != nil {
		return statements{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return statements{}, fmt.Errorf("commit column-type read of %s.%s: %w", v.shadow.Schema(), v.shadow.ShadowTable(), err)
	}
	schema, source, shadow := v.shadow.Schema(), v.shadow.SourceTable(), v.shadow.ShadowTable()
	conversions := v.shadow.Conversions()
	return statements{
		source: sideSQL{
			digest: digestSQL(v.target, schema, source, types, conversions),
			rows:   rowsSQL(v.target, schema, source, types, conversions),
		},
		shadow: sideSQL{
			digest: digestSQL(v.target, schema, shadow, types, nil),
			rows:   rowsSQL(v.target, schema, shadow, types, nil),
		},
	}, nil
}
