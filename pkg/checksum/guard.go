package checksum

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// sqlstateUndefinedTable is the SQLSTATE the server raises when a statement
// names a relation that does not exist.
const sqlstateUndefinedTable = "42P01"

// begin opens the read-only transaction one chunk's two digests run in and
// guards it. The transaction is REPEATABLE READ so its two reads see one
// snapshot: the source and the shadow are compared as they stood at the
// same instant, and the snapshot is taken by the first query after both
// relations are locked, so no rename or drop can slip between the lock and
// the reads.
func (v *Verifier) begin(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin verification of %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	if err := v.guard(ctx, tx); err != nil {
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			return nil, errors.Join(err, fmt.Errorf("roll back verification of %s.%s: %w", v.target.Schema(), v.target.Table(), rollbackErr))
		}
		return nil, err
	}
	return tx, nil
}

// guard prepares the transaction every read runs in: it bounds it, puts the
// catalog alone on its search_path, puts it under the source owner's role,
// locks both relations by name, and then confirms from this connection that
// the lock session's backend still holds the table and that the source and
// shadow are still the relations the proofs describe. Holding even ACCESS
// SHARE keeps a DROP or rename from completing until the transaction ends,
// so the identity check stays true for every statement after it.
func (v *Verifier) guard(ctx context.Context, tx pgx.Tx) error {
	if err := setVerifySession(ctx, tx, v.target.OwnerRole(), v.opts); err != nil {
		return err
	}
	if err := v.holdRelations(ctx, tx); err != nil {
		return err
	}
	if err := v.confirmLock(ctx, tx); err != nil {
		return err
	}
	return v.confirmRelations(ctx, tx)
}

// setVerifySession bounds the transaction, restricts its search_path to the
// catalog so every unqualified operator, cast, and type name resolves there
// (CO-9), and puts it in the owner's shoes. SET LOCAL cannot take bind
// parameters; the timeouts are integer milliseconds.
func setVerifySession(ctx context.Context, tx pgx.Tx, owner string, opts Options) error {
	// INV: LK-2
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10) +
		"; " + dbconn.LocalSearchPath("pg_catalog")
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return fmt.Errorf("set verification budgets: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{owner}.Sanitize()); err != nil {
		return fmt.Errorf("set owner role %s: %w", owner, err)
	}
	return nil
}

// holdRelations takes the weakest table lock on the source and the shadow
// for the rest of the transaction. A name that no longer resolves is a
// relation replaced or gone since its proof was minted.
func (v *Verifier) holdRelations(ctx context.Context, tx pgx.Tx) error {
	source := pgx.Identifier{v.shadow.Schema(), v.shadow.SourceTable()}.Sanitize()
	shadow := pgx.Identifier{v.shadow.Schema(), v.shadow.ShadowTable()}.Sanitize()
	_, err := tx.Exec(ctx, "LOCK TABLE "+source+", "+shadow+" IN ACCESS SHARE MODE")
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable {
		// INV: ST-6
		return fmt.Errorf("%w (ST-6): %s or its shadow %s no longer exists: %w", ErrInvariantViolation, source, shadow, err)
	}
	return fmt.Errorf("lock %s and %s for verification: %w", source, shadow, err)
}

// confirmLock asks the server, on the connection about to read, whether the
// lock session's own backend holds the table. A server that answers "no one"
// or "another backend" has contradicted the session the verifier trusts,
// and that is the verifier's invariant violation; a lookup that got no
// answer is the connection's error, reported as such.
func (v *Verifier) confirmLock(ctx context.Context, conn dbconn.AdvisoryLockHolder) error {
	err := v.lock.Confirm(ctx, conn)
	if err == nil {
		return nil
	}
	if lockDenied(err) {
		// INV: LK-1
		return fmt.Errorf("%w (LK-1): %w", ErrInvariantViolation, err)
	}
	return fmt.Errorf("confirm the table lock from the verification transaction: %w", err)
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

// relationOIDsSQL resolves the source ($2) and shadow ($3) in schema $1 by
// explicit qualification; a missing relation scans as NULL.
const relationOIDsSQL = `
	SELECT
		(SELECT c.oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2),
		(SELECT c.oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $3)`

// confirmRelations refuses to read when the source or the shadow is no
// longer the relation its proof was minted for: a table dropped and
// recreated under the same name has a new OID, and a digest of it would
// verify a table nobody proved.
func (v *Verifier) confirmRelations(ctx context.Context, tx pgx.Tx) error {
	var sourceOID, shadowOID *uint32
	err := tx.QueryRow(ctx, relationOIDsSQL, v.shadow.Schema(), v.shadow.SourceTable(), v.shadow.ShadowTable()).Scan(&sourceOID, &shadowOID)
	if err != nil {
		return fmt.Errorf("resolve %s.%s and its shadow %s: %w", v.shadow.Schema(), v.shadow.SourceTable(), v.shadow.ShadowTable(), err)
	}
	// INV: ST-6
	if err := confirmRelation("source", v.shadow.Schema(), v.shadow.SourceTable(), v.shadow.SourceOID(), sourceOID); err != nil {
		return err
	}
	return confirmRelation("shadow", v.shadow.Schema(), v.shadow.ShadowTable(), v.shadow.ShadowOID(), shadowOID)
}

func confirmRelation(role, schema, table string, proven uint32, found *uint32) error {
	if found == nil {
		return fmt.Errorf("%w (ST-6): %s %s.%s no longer exists", ErrInvariantViolation, role, schema, table)
	}
	if *found != proven {
		return fmt.Errorf("%w (ST-6): %s %s.%s is relation %d, proof was minted for relation %d", ErrInvariantViolation, role, schema, table, *found, proven)
	}
	return nil
}
