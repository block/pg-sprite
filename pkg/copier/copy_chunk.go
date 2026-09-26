package copier

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
)

// copyChunk copies one chunk in its own bounded transaction under the
// source owner's role: it confirms from this connection that the lock
// session's backend still holds the table and that the source and shadow are
// still the relations the proofs describe, then inserts the chunk's live rows
// into the shadow without overwriting any row already there. It returns the
// number of rows the insert added.
func (c *Copier) copyChunk(ctx context.Context, pool *pgxpool.Pool, chunk Chunk) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin copy of chunk [%d, %d]: %w", chunk.Lower(), chunk.Upper(), err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setCopySession(ctx, tx, c.target, c.opts); err != nil {
		return 0, err
	}
	if err := c.confirmLock(ctx, tx); err != nil {
		return 0, err
	}
	if err := c.confirmRelations(ctx, tx); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, c.sql, chunk.Lower(), chunk.Upper())
	if err != nil {
		return 0, fmt.Errorf("copy chunk [%d, %d] of %s.%s into %s: %w", chunk.Lower(), chunk.Upper(), c.target.Schema(), c.target.Table(), c.shadow.ShadowTable(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit chunk [%d, %d] of %s.%s: %w", chunk.Lower(), chunk.Upper(), c.target.Schema(), c.target.Table(), err)
	}
	return tag.RowsAffected(), nil
}

// setCopySession bounds the transaction and puts it in the owner's shoes.
// SET LOCAL cannot take bind parameters; the timeouts are integer
// milliseconds. Every identifier the copy touches is schema-qualified, so
// the session's search_path plays no part.
func setCopySession(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, opts Options) error {
	// INV: LK-2
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return fmt.Errorf("set chunk copy budgets: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{target.OwnerRole()}.Sanitize()); err != nil {
		return fmt.Errorf("set owner role %s: %w", target.OwnerRole(), err)
	}
	return nil
}

// confirmLock asks the server, on the connection about to write, whether the
// lock session's own backend holds the table. Any other answer is an
// invariant violation for the copier, whichever way dbconn reports it.
func (c *Copier) confirmLock(ctx context.Context, tx pgx.Tx) error {
	// INV: LK-1
	if err := c.lock.Confirm(ctx, tx); err != nil {
		return fmt.Errorf("%w (LK-1): %w", ErrInvariantViolation, err)
	}
	return nil
}

// confirmRelations refuses to write when the source or the shadow is no
// longer the relation its proof was minted for: a table dropped and recreated
// under the same name has a new OID, and a copy into it would be a copy into
// a table nobody proved.
func (c *Copier) confirmRelations(ctx context.Context, tx pgx.Tx) error {
	var sourceOID, shadowOID *uint32
	err := tx.QueryRow(ctx, relationOIDsSQL, c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.ShadowTable()).Scan(&sourceOID, &shadowOID)
	if err != nil {
		return fmt.Errorf("resolve %s.%s and its shadow %s: %w", c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.ShadowTable(), err)
	}
	// INV: ST-6
	if err := confirmRelation("source", c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.SourceOID(), sourceOID); err != nil {
		return err
	}
	return confirmRelation("shadow", c.shadow.Schema(), c.shadow.ShadowTable(), c.shadow.ShadowOID(), shadowOID)
}

// relationOIDsSQL resolves the source ($2) and shadow ($3) in schema $1 by
// explicit qualification; a missing relation scans as NULL.
const relationOIDsSQL = `
	SELECT
		(SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2),
		(SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $3)`

func confirmRelation(role, schema, table string, proven uint32, found *uint32) error {
	if found == nil {
		return fmt.Errorf("%w (ST-6): %s %s.%s no longer exists", ErrInvariantViolation, role, schema, table)
	}
	if *found != proven {
		return fmt.Errorf("%w (ST-6): %s %s.%s is relation %d, proof was minted for relation %d", ErrInvariantViolation, role, schema, table, *found, proven)
	}
	return nil
}

// copySQL is the one statement every chunk runs: insert the shared columns
// of the source rows whose key lies in the closed range [$1, $2] into the
// shadow, skipping any key the shadow already holds — the applier always
// overwrites and the copier never does, which is what lets the two run
// concurrently (CO-4). The bounds are declared bigint whatever the key's
// integer type, as the chunker's boundary query declares them, so a bound
// outside a smaller key type's range can still be sent and the primary-key
// index still serves the range scan.
func copySQL(target preflight.CopySwapTarget, shadow Shadow) string {
	columns := make([]string, 0, len(shadow.CopyColumns()))
	for _, column := range shadow.CopyColumns() {
		columns = append(columns, pgx.Identifier{column}.Sanitize())
	}
	list := strings.Join(columns, ", ")
	key := pgx.Identifier{target.PKColumn()}.Sanitize()
	return "INSERT INTO " + pgx.Identifier{shadow.Schema(), shadow.ShadowTable()}.Sanitize() +
		" (" + list + ")" +
		" SELECT " + list +
		" FROM " + pgx.Identifier{shadow.Schema(), shadow.SourceTable()}.Sanitize() +
		" WHERE " + key + " BETWEEN $1::bigint AND $2::bigint" +
		" ON CONFLICT (" + key + ") DO NOTHING"
}
