package copier

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/internal/chunksql"
	"github.com/block/pg-sprite/pkg/preflight"
)

// sqlstateUndefinedTable is the SQLSTATE the server raises when a statement
// names a relation that does not exist.
const sqlstateUndefinedTable = "42P01"

// copyChunk copies one chunk in its own guarded transaction: it inserts the
// chunk's live rows into the shadow without overwriting any row already
// there and returns the number of rows the insert added.
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
	if err := c.guard(ctx, tx); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, c.sql, chunk.Lower(), chunk.Upper())
	if err != nil {
		return 0, fmt.Errorf("copy chunk [%d, %d] of %s.%s into %s: %w", chunk.Lower(), chunk.Upper(), c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.ShadowTable(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit chunk [%d, %d] of %s.%s: %w", chunk.Lower(), chunk.Upper(), c.target.Schema(), c.target.Table(), err)
	}
	return tag.RowsAffected(), nil
}

// guard prepares the transaction every write into the shadow runs in: it
// bounds it, puts the catalog alone on its search_path, puts it under the
// source owner's role, and confirms from this connection that the lock
// session's backend still holds the table and that the source and shadow
// are still the relations the proofs describe. Both relations are locked by
// name before their identity is checked, so nothing can replace either one
// between the check and the statements that follow it in the same
// transaction.
func (c *Copier) guard(ctx context.Context, tx pgx.Tx) error {
	if err := setCopySession(ctx, tx, c.target, c.opts); err != nil {
		return err
	}
	if err := c.confirmLock(ctx, tx); err != nil {
		return err
	}
	if err := c.holdRelations(ctx, tx); err != nil {
		return err
	}
	return c.confirmRelations(ctx, tx)
}

// setCopySession bounds the transaction, restricts its search_path to the
// catalog, and puts it in the owner's shoes. Every relation the copy names
// is schema-qualified, but the key range's BETWEEN resolves its operators
// through the search_path, and a shadow default or trigger fires under it
// too; pinning the catalog keeps a schema ahead of it on the caller's pool
// from answering either (CO-9), and gives the verifier's repair, which runs
// the same statement under the same pin, the same session as the copy.
func setCopySession(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, opts Options) error {
	if err := setBudgets(ctx, tx, opts); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return fmt.Errorf("set copy search_path: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{target.OwnerRole()}.Sanitize()); err != nil {
		return fmt.Errorf("set owner role %s: %w", target.OwnerRole(), err)
	}
	return nil
}

// setBudgets bounds every lock wait and statement in the transaction with
// the copier's own timeouts, whatever session defaults the caller's pool
// carries. SET LOCAL cannot take bind parameters; the timeouts are integer
// milliseconds.
func setBudgets(ctx context.Context, tx pgx.Tx, opts Options) error {
	// INV: LK-2
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return fmt.Errorf("set copy budgets: %w", err)
	}
	return nil
}

// confirmLock asks the server, on the connection about to write, whether the
// lock session's own backend holds the table. A server that answers "no one"
// or "another backend" has contradicted the session the copier trusts, and
// that is the copier's invariant violation; a lookup that got no answer is
// the connection's error, reported as such.
func (c *Copier) confirmLock(ctx context.Context, conn dbconn.AdvisoryLockHolder) error {
	err := c.lock.Confirm(ctx, conn)
	if err == nil {
		return nil
	}
	if lockDenied(err) {
		// INV: LK-1
		return fmt.Errorf("%w (LK-1): %w", ErrInvariantViolation, err)
	}
	return fmt.Errorf("confirm the table lock from the copy transaction: %w", err)
}

// lockDenied reports whether a Confirm error is the server's word that the
// session does not hold the table, in either of the two forms dbconn gives
// it.
func lockDenied(err error) bool {
	if errors.Is(err, dbconn.ErrTableLockNotHeld) {
		return true
	}
	var heldElsewhere *dbconn.TableLockHeldError
	return errors.As(err, &heldElsewhere)
}

// holdRelations takes the weakest table lock on the source and the shadow
// for the rest of the transaction. LOCK TABLE resolves the names as the
// statements after it will, and holding even ACCESS SHARE keeps a DROP or
// rename from completing until the transaction ends, so the identity check
// that follows stays true for every statement in it. A name that no longer
// resolves is a relation replaced or gone since its proof was minted.
func (c *Copier) holdRelations(ctx context.Context, tx pgx.Tx) error {
	source := pgx.Identifier{c.shadow.Schema(), c.shadow.SourceTable()}.Sanitize()
	shadow := pgx.Identifier{c.shadow.Schema(), c.shadow.ShadowTable()}.Sanitize()
	_, err := tx.Exec(ctx, "LOCK TABLE "+source+", "+shadow+" IN ACCESS SHARE MODE")
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable {
		// INV: ST-6
		return fmt.Errorf("%w (ST-6): %s or its shadow %s no longer exists: %w", ErrInvariantViolation, source, shadow, err)
	}
	return fmt.Errorf("lock %s and %s for the copy: %w", source, shadow, err)
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
// explicit qualification; a missing relation scans as NULL. Every catalog
// name in it is qualified as well as pinned by the session: a decoy
// pg_class ahead of the catalog on the caller's path must not answer the
// identity check (CO-9).
const relationOIDsSQL = `
	SELECT
		(SELECT c.oid
		   FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2),
		(SELECT c.oid
		   FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $3)`

func confirmRelation(role, schema, table string, proven uint32, found *uint32) error {
	if found == nil {
		return fmt.Errorf("%w (ST-6): %s %s.%s no longer exists", ErrInvariantViolation, role, schema, table)
	}
	if *found != proven {
		return fmt.Errorf("%w (ST-6): %s %s.%s is relation %d, proof was minted for relation %d", ErrInvariantViolation, role, schema, table, *found, proven)
	}
	return nil
}

// copySQL is the one statement every chunk runs, the chunk insert shared
// with the verifier's repair, built for this target and shadow.
func copySQL(target preflight.CopySwapTarget, shadow Shadow) string {
	return chunksql.Insert(shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), target.PKColumn(), shadow.CopyColumns())
}
