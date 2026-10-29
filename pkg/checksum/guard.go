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
	"github.com/block/pg-sprite/pkg/internal/chunksql"
)

// sqlstateUndefinedTable is the SQLSTATE the server raises when a statement
// names a relation that does not exist.
const sqlstateUndefinedTable = "42P01"

// snapshotRead is the transaction one chunk's two digests run in: read-only
// and REPEATABLE READ, so its two reads see one snapshot — the source and
// the shadow are compared as they stood at the same instant — and the
// snapshot is taken by the first query after both relations are locked, so
// no rename or drop can slip between the lock and the reads.
func snapshotRead() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
}

// repairWrite is the transaction a recopy runs in: read-write and READ
// COMMITTED whatever the server or role configures, so the resolution
// check repeated after the copies reads a snapshot taken after them and
// sees a catalog change they resolved through (CO-9). Nothing in a recopy
// relies on one snapshot across its statements: each chunk's copy reads
// the source as one statement, and the stream or the reread settles what
// changed after it.
func repairWrite() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}
}

// begin opens a transaction with the given options and guards it.
func (v *Verifier) begin(ctx context.Context, pool *pgxpool.Pool, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, options)
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

// guard prepares the transaction every read and every repair runs in: it
// bounds it, sets the search_path the copy statement runs under, puts it
// under the source owner's role, locks both relations by name, and then confirms from
// this connection that the lock session's backend still holds the table and
// that the source and shadow are still the relations the proofs describe.
// Holding even ACCESS SHARE keeps a DROP or rename from completing until the
// transaction ends, so the identity check stays true for every statement
// after it. Last, for a copy that evaluates conversion expressions, it
// confirms that the catalog they resolve in is still the one the build saw
// (CO-9).
func (v *Verifier) guard(ctx context.Context, tx pgx.Tx) error {
	if err := setVerifySession(ctx, tx, v.target.OwnerRole(), v.searchPath, v.opts); err != nil {
		return err
	}
	if err := v.holdRelations(ctx, tx); err != nil {
		return err
	}
	if err := v.confirmLock(ctx, tx); err != nil {
		return err
	}
	if err := v.confirmRelations(ctx, tx); err != nil {
		return err
	}
	return v.confirmResolution(ctx, tx)
}

// confirmResolution refuses to read or repair when the functions,
// operators, types, or collations the conversion expressions resolve among
// are no longer the ones the shadow build digested: the digest's source
// side and the recopy evaluate the expressions afresh, so an object
// created, replaced, or dropped under one of their names since the build
// would make this pass compare the shadow against, or recopy it from, a
// rendering the gated statement never produced. A copy without conversions
// has nothing to confirm.
func (v *Verifier) confirmResolution(ctx context.Context, tx pgx.Tx) error {
	if len(v.shadow.Conversions()) == 0 {
		return nil
	}
	found, err := chunksql.ResolutionFingerprint(ctx, tx, v.shadow.Schema(), v.shadow.Conversions())
	if err != nil {
		return err
	}
	// INV: CO-9
	if found != v.shadow.ConversionFingerprint() {
		return fmt.Errorf("%w (CO-9): the objects the conversion expressions for %s.%s resolve among have changed since the shadow %s was built: fingerprint %s, proof carries %s", ErrInvariantViolation, v.shadow.Schema(), v.shadow.SourceTable(), v.shadow.ShadowTable(), found, v.shadow.ConversionFingerprint())
	}
	return nil
}

// setVerifySession bounds the transaction, pins the output settings under
// which one value has more than one text rendering, sets the search_path,
// and puts it in the owner's shoes. The search_path is the one chunksql
// chose for the copy statement: the catalog alone when the copy carries no
// conversions, so every unqualified operator, cast, and type name resolves
// there (CO-9); the build's path when it does, so a recopy's conversion
// expressions and the digests' type names resolve as the gated statement's
// did, with pg_catalog still ahead of both. The digest hashes each row's
// text rendering, and for float4, float8, and the geometric types that
// rendering follows extra_float_digits: at zero or below the server rounds
// to fifteen significant digits, so two floats that differ in their last
// digits would render, and hash, the same. The digest's two sides share the
// session, so a setting that only changes a value's spelling (DateStyle,
// IntervalStyle, TimeZone) cannot part them; a conversion that renders a
// float or a bytea to text is different, because the chunk copy evaluated
// it in a session of its own and the shadow holds that session's text.
// chunksql.Rendering is the pin the copy, this pass's digest and recopy,
// and the flush share, so the source side of the digest renders the
// conversion as the copy did. SET LOCAL cannot take bind parameters; the
// timeouts are integer milliseconds.
func setVerifySession(ctx context.Context, tx pgx.Tx, owner, searchPath string, opts Options) error {
	// INV: LK-2
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10) +
		"; " + chunksql.Rendering +
		"; " + searchPath
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
