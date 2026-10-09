package applier

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// sqlstateUndefinedTable is the SQLSTATE the server raises when a statement
// names a relation that does not exist.
const sqlstateUndefinedTable = "42P01"

// begin opens the flush's read-write transaction and guards it.
func (f *Flusher) begin(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, fmt.Errorf("begin flush into %s.%s: %w", f.shadow.Schema(), f.shadow.ShadowTable(), err)
	}
	if err := f.guard(ctx, tx); err != nil {
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			return nil, errors.Join(err, fmt.Errorf("roll back flush into %s.%s: %w", f.shadow.Schema(), f.shadow.ShadowTable(), rollbackErr))
		}
		return nil, err
	}
	return tx, nil
}

// guard prepares the transaction every flush runs in: it bounds it, puts
// the catalog alone on its search_path, puts it under the source owner's
// role, locks both relations by name, and then confirms from this
// connection that the lock session's backend still holds the table and that
// the source and shadow are still the relations the proofs describe.
// Holding even ACCESS SHARE keeps a DROP or rename from completing until
// the transaction ends, so the identity check stays true for every
// statement after it.
func (f *Flusher) guard(ctx context.Context, tx pgx.Tx) error {
	if err := setFlushSession(ctx, tx, f.target.OwnerRole(), f.opts); err != nil {
		return err
	}
	if err := f.holdRelations(ctx, tx); err != nil {
		return err
	}
	if err := f.confirmLock(ctx, tx); err != nil {
		return err
	}
	return f.confirmRelations(ctx, tx)
}

// setFlushSession bounds the transaction, pins the one output setting that
// can merge two distinct values, restricts its search_path to the catalog
// so every unqualified operator, cast, and type name resolves there (CO-9),
// and puts it in the owner's shoes. A marker is completed by reading a
// value as text and binding that text back, and for float4, float8, and the
// geometric types that rendering follows extra_float_digits: at zero or
// below the server rounds to fifteen significant digits, so a completed
// float could differ in its last digits from the one it stood for. The
// maximum of 3 renders every float exactly whatever the database or role
// configures. SET LOCAL cannot take bind parameters; the timeouts are
// integer milliseconds.
func setFlushSession(ctx context.Context, tx pgx.Tx, owner string, opts Options) error {
	// INV: LK-2
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10) +
		"; SET LOCAL extra_float_digits = 3" +
		"; " + dbconn.LocalSearchPath("pg_catalog")
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return fmt.Errorf("set flush budgets: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{owner}.Sanitize()); err != nil {
		return fmt.Errorf("set owner role %s: %w", owner, err)
	}
	return nil
}

// holdRelations takes the weakest table lock on the source and the shadow
// for the rest of the transaction. A name that no longer resolves is a
// relation replaced or gone since its proof was minted.
func (f *Flusher) holdRelations(ctx context.Context, tx pgx.Tx) error {
	source := f.sourceRelation()
	shadow := f.shadowRelation()
	_, err := tx.Exec(ctx, "LOCK TABLE "+source+", "+shadow+" IN ACCESS SHARE MODE")
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable {
		// INV: ST-6
		return fmt.Errorf("%w (ST-6): %s or its shadow %s no longer exists: %w", ErrInvariantViolation, source, shadow, err)
	}
	return fmt.Errorf("lock %s and %s for the flush: %w", source, shadow, err)
}

// confirmLock asks the server, on the connection about to write, whether
// the lock session's own backend holds the table. A server that answers "no
// one" or "another backend" has contradicted the session the flusher
// trusts, and that is the flusher's invariant violation; a lookup that got
// no answer is the connection's error, reported as such.
func (f *Flusher) confirmLock(ctx context.Context, conn dbconn.AdvisoryLockHolder) error {
	err := f.lock.Confirm(ctx, conn)
	if err == nil {
		return nil
	}
	if lockDenied(err) {
		// INV: LK-1
		return fmt.Errorf("%w (LK-1): %w", ErrInvariantViolation, err)
	}
	return fmt.Errorf("confirm the table lock from the flush transaction: %w", err)
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

// confirmRelations refuses to write when the source or the shadow is no
// longer the relation its proof was minted for: a table dropped and
// recreated under the same name has a new OID, and a flush into it would
// write a table nobody proved.
func (f *Flusher) confirmRelations(ctx context.Context, tx pgx.Tx) error {
	var sourceOID, shadowOID *uint32
	err := tx.QueryRow(ctx, relationOIDsSQL, f.shadow.Schema(), f.shadow.SourceTable(), f.shadow.ShadowTable()).Scan(&sourceOID, &shadowOID)
	if err != nil {
		return fmt.Errorf("resolve %s.%s and its shadow %s: %w", f.shadow.Schema(), f.shadow.SourceTable(), f.shadow.ShadowTable(), err)
	}
	// INV: ST-6
	if err := confirmRelation("source", f.shadow.Schema(), f.shadow.SourceTable(), f.shadow.SourceOID(), sourceOID); err != nil {
		return err
	}
	return confirmRelation("shadow", f.shadow.Schema(), f.shadow.ShadowTable(), f.shadow.ShadowOID(), shadowOID)
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

// lockLost reports the table lock's loss as the flusher's invariant
// violation wrapping ErrTableLockLost, or nil while the session still
// holds it.
func (f *Flusher) lockLost() error {
	lost := f.lock.Err()
	if lost == nil {
		return nil
	}
	// INV: LK-1
	return fmt.Errorf("%w (LK-1): %w: %w", ErrInvariantViolation, ErrTableLockLost, lost)
}

// lostOr returns the lock's loss when the session has reported one — a
// write cancelled by Bind is explained by the loss, not by its own error —
// and err otherwise.
func (f *Flusher) lostOr(err error) error {
	if lost := f.lockLost(); lost != nil {
		return lost
	}
	return err
}

// checkShadow refuses a shadow proof that does not describe the proven
// target's shadow, with the copier's rules: the flusher writes into the
// relation the copier copied into, so a proof the copier would refuse
// describes nothing the flusher may write.
func checkShadow(target preflight.CopySwapTarget, shadow copier.Shadow) error {
	// INV: ST-6
	if shadow == nil {
		return fmt.Errorf("%w (ST-6): flush requires a built shadow", ErrInvariantViolation)
	}
	if shadow.ShadowTable() == "" {
		return fmt.Errorf("%w (ST-6): shadow proof is empty", ErrInvariantViolation)
	}
	if shadow.Schema() != target.Schema() || shadow.SourceTable() != target.Table() {
		return fmt.Errorf("%w (ST-6): shadow is for %s.%s, proof is for %s.%s", ErrInvariantViolation, shadow.Schema(), shadow.SourceTable(), target.Schema(), target.Table())
	}
	if shadow.ShadowTable() == shadow.SourceTable() {
		return fmt.Errorf("%w (ST-6): shadow of %s.%s is the source table itself", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if shadow.SourceOID() == 0 || shadow.ShadowOID() == 0 {
		return fmt.Errorf("%w (ST-6): shadow proof for %s.%s carries no relation OIDs", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if shadow.SourceOID() == shadow.ShadowOID() {
		return fmt.Errorf("%w (ST-6): shadow proof for %s.%s names relation %d as both source and shadow", ErrInvariantViolation, target.Schema(), target.Table(), shadow.SourceOID())
	}
	if !slices.Contains(shadow.CopyColumns(), target.PKColumn()) {
		return fmt.Errorf("%w (ST-6): shadow copy columns for %s.%s do not include the primary key %s", ErrInvariantViolation, target.Schema(), target.Table(), target.PKColumn())
	}
	return nil
}

// requireTableLock refuses to flush without the per-table lock that keeps
// a second engine instance off the same table: the session must exist,
// carry a populated proof for the proven table, and not have reported loss.
func requireTableLock(lock *dbconn.TableLockSession, target preflight.CopySwapTarget) error {
	// INV: LK-1
	if lock == nil {
		return fmt.Errorf("%w (LK-1): flush requires a table lock session", ErrInvariantViolation)
	}
	held := lock.Lock()
	if held.Table() == "" {
		return fmt.Errorf("%w (LK-1): table lock proof is empty", ErrInvariantViolation)
	}
	if held.Schema() != target.Schema() || held.Table() != target.Table() {
		return fmt.Errorf("%w (LK-1): table lock is for %s.%s, proof is for %s.%s", ErrInvariantViolation, held.Schema(), held.Table(), target.Schema(), target.Table())
	}
	if err := lock.Err(); err != nil {
		return fmt.Errorf("%w (LK-1): %w before the flush: %w", ErrInvariantViolation, ErrTableLockLost, err)
	}
	return nil
}
