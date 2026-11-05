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

// lockMode is a table lock mode as LOCK TABLE spells it.
type lockMode string

const (
	// lockAccessShare is the mode a read holds: it conflicts with nothing
	// short of ACCESS EXCLUSIVE and keeps a DROP or rename from completing.
	lockAccessShare lockMode = "ACCESS SHARE"
	// lockRowExclusive is the mode an INSERT, UPDATE, or DELETE holds on
	// the table it writes until its commit completes.
	lockRowExclusive lockMode = "ROW EXCLUSIVE"
	// lockShareRowExclusive is the resume fence's mode: it conflicts with
	// the ROW EXCLUSIVE a writer holds and with itself, so it waits behind
	// every straggling write and behind another fence, and it still admits
	// every ACCESS SHARE reader. A transaction that takes it as its first
	// lock on the shadow never asks the server to strengthen a lock it
	// already holds, so two fences meeting on the shadow queue instead of
	// deadlocking.
	lockShareRowExclusive lockMode = "SHARE ROW EXCLUSIVE"
)

// copyChunk copies one chunk in its own guarded transaction: it inserts the
// chunk's live rows into the shadow without overwriting any row already
// there and returns the number of rows the insert added. The insert is one
// statement and relies on no snapshot beyond its own.
func (c *Copier) copyChunk(ctx context.Context, pool *pgxpool.Pool, chunk Chunk) (int64, error) {
	var tag pgconn.CommandTag
	err := c.chunkTx(ctx, pool, chunk, func(tx pgx.Tx) error {
		var err error
		tag, err = tx.Exec(ctx, c.sql, chunk.Lower(), chunk.Upper())
		if err != nil {
			return fmt.Errorf("copy chunk [%d, %d] of %s.%s into %s: %w", chunk.Lower(), chunk.Upper(), c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.ShadowTable(), err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// chunkTx runs write, one chunk's insert into the shadow, in its own guarded
// transaction and commits it. A copy that evaluates conversion expressions
// confirms their resolution once more after the write and before the
// commit (CO-9), so a catalog change that committed between the guard's
// check and the write rolls the chunk back. The transaction is READ
// COMMITTED whatever the server or role configures, so that repeated read
// takes a snapshot after the write's.
func (c *Copier) chunkTx(ctx context.Context, pool *pgxpool.Pool, chunk Chunk, write func(tx pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return fmt.Errorf("begin copy of chunk [%d, %d]: %w", chunk.Lower(), chunk.Upper(), err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := c.guard(ctx, tx, lockRowExclusive); err != nil {
		return err
	}
	if err := write(tx); err != nil {
		return err
	}
	if err := c.confirmResolution(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chunk [%d, %d] of %s.%s: %w", chunk.Lower(), chunk.Upper(), c.target.Schema(), c.target.Table(), err)
	}
	return nil
}

// guard prepares the transaction every write into the shadow runs in: it
// bounds it, sets the search_path the copy statement needs, puts it under
// the source owner's role, and confirms from this connection that the lock
// session's backend still holds the table and that the source and shadow
// are still the relations the proofs describe. Both relations are locked by
// name before their identity is checked, so nothing can replace either one
// between the check and the statements that follow it in the same
// transaction. The shadow is locked in shadowMode — the mode the write that
// follows holds, or the fence's — before the table lock is confirmed, so a
// transaction that confirmed it already holds the lock a resumed run's fence
// waits on (CO-4). Last, for a copy that evaluates conversion expressions,
// it confirms that the catalog they resolve in is still the one the build
// saw (CO-9).
func (c *Copier) guard(ctx context.Context, tx pgx.Tx, shadowMode lockMode) error {
	if err := setCopySession(ctx, tx, c.target, c.searchPath, c.opts); err != nil {
		return err
	}
	if err := c.holdRelations(ctx, tx, shadowMode); err != nil {
		return err
	}
	if err := c.confirmLock(ctx, tx); err != nil {
		return err
	}
	if err := c.confirmRelations(ctx, tx); err != nil {
		return err
	}
	return c.confirmResolution(ctx, tx)
}

// confirmResolution refuses to write when the functions, operators, types,
// or collations the conversion expressions resolve among are no longer the
// ones the shadow build digested: the copy statement resolves the
// expressions' unqualified names afresh in every chunk, so an object
// created, replaced, or dropped under one of those names since the build
// could make this chunk evaluate them differently from the gated statement
// without any statement failing. The fingerprint is read in the chunk's
// own transaction, but by a statement of its own, so a change committed
// after the read and before the insert would still reach the insert: the
// guard's read narrows that window, and the read copyChunk repeats after
// the insert rolls back a chunk that resolved through a change, since
// under READ COMMITTED, which copyChunk pins, that read sees every change
// the insert could have resolved. A change made and undone again between
// the two reads is seen by neither; the verifier's recomputation of the
// source side is what catches a chunk such a change reached. A copy
// without conversions has nothing to confirm.
func (c *Copier) confirmResolution(ctx context.Context, tx pgx.Tx) error {
	if len(c.shadow.Conversions()) == 0 {
		return nil
	}
	found, err := chunksql.ResolutionFingerprint(ctx, tx, c.shadow.Schema(), c.shadow.Conversions())
	if err != nil {
		return err
	}
	// INV: CO-9
	if found != c.shadow.ConversionFingerprint() {
		return refuse(CauseConversionDrift, nil, "the objects the conversion expressions for %s.%s resolve among have changed since the shadow %s was built: fingerprint %s, proof carries %s", c.shadow.Schema(), c.shadow.SourceTable(), c.shadow.ShadowTable(), found, c.shadow.ConversionFingerprint())
	}
	return nil
}

// setCopySession bounds the transaction, pins the output settings a
// conversion's text may follow, sets its search_path, and puts it in the
// owner's shoes. The search_path is the one chunksql chose for the
// statement: the catalog alone for a copy without conversions, since every
// relation the copy names is schema-qualified and pinning the catalog keeps
// a schema ahead of it on the caller's pool from answering the key range's
// operators or a shadow default or trigger (CO-9); the build's path for a
// copy whose conversion expressions must resolve as the gated statement
// did. The output settings are chunksql.Rendering, the same pin the
// verifier and the flush set, so a float or bytea a conversion renders to
// text lands from the copy as the same text the flush and the recopy
// write; a copy without conversions renders nothing as text, and the pin
// costs it nothing. The verifier's repair runs the same statement under the
// same path and pin, so a repair has the same session as the copy.
func setCopySession(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, searchPath string, opts Options) error {
	if err := setBudgets(ctx, tx, opts); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, chunksql.Rendering); err != nil {
		return fmt.Errorf("set copy output settings: %w", err)
	}
	if _, err := tx.Exec(ctx, searchPath); err != nil {
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
// each answer is its own refusal cause; a lookup that got no answer is the
// connection's error, reported as such.
func (c *Copier) confirmLock(ctx context.Context, conn dbconn.AdvisoryLockHolder) error {
	err := c.lock.Confirm(ctx, conn)
	if err == nil {
		return nil
	}
	// INV: LK-1
	if errors.Is(err, dbconn.ErrTableLockNotHeld) {
		return refuse(CauseLockUnconfirmed, []error{err}, "confirming the table lock from the copy transaction")
	}
	var heldElsewhere *dbconn.TableLockHeldError
	if errors.As(err, &heldElsewhere) {
		return refuse(CauseLockHeldElsewhere, []error{err}, "the copy transaction sees the table lock held by another backend, not the lock session's backend %d", c.lock.BackendPID())
	}
	return fmt.Errorf("confirm the table lock from the copy transaction: %w", err)
}

// holdRelations takes ACCESS SHARE on the source and shadowMode on the
// shadow, for the rest of the transaction. LOCK TABLE resolves the names as
// the statements after it will, and holding even ACCESS SHARE keeps a DROP or
// rename from completing until the transaction ends, so the identity check
// that follows stays true for every statement in it. The shadow lock is the
// transaction's first on the shadow and at least the mode its write holds,
// so it never strengthens a lock it already holds; the guard takes it before
// it confirms the table lock, so a transaction that has confirmed it is
// already one the resumed run's fence waits behind, whatever it is paused
// before. The source is locked first, the order the cutover's one LOCK TABLE
// statement takes them in.
func (c *Copier) holdRelations(ctx context.Context, tx pgx.Tx, shadowMode lockMode) error {
	source := pgx.Identifier{c.shadow.Schema(), c.shadow.SourceTable()}.Sanitize()
	shadow := pgx.Identifier{c.shadow.Schema(), c.shadow.ShadowTable()}.Sanitize()
	if err := holdRelation(ctx, tx, source, lockAccessShare); err != nil {
		return err
	}
	// INV: CO-4
	return holdRelation(ctx, tx, shadow, shadowMode)
}

// holdRelation locks one relation by its sanitized name in mode. A name that
// no longer resolves is a relation replaced or gone since its proof was
// minted.
func holdRelation(ctx context.Context, tx pgx.Tx, relation string, mode lockMode) error {
	_, err := tx.Exec(ctx, "LOCK TABLE "+relation+" IN "+string(mode)+" MODE")
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable {
		// INV: ST-6
		return refuse(CauseRelationReplaced, []error{err}, "%s no longer exists", relation)
	}
	return fmt.Errorf("lock %s in %s mode for the copy: %w", relation, mode, err)
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
		return refuse(CauseRelationReplaced, nil, "%s %s.%s no longer exists", role, schema, table)
	}
	if *found != proven {
		return refuse(CauseRelationReplaced, nil, "%s %s.%s is relation %d, proof was minted for relation %d", role, schema, table, *found, proven)
	}
	return nil
}

// copySQL is the one statement every chunk runs, the chunk insert shared
// with the verifier's repair, built for this target and shadow.
func copySQL(target preflight.CopySwapTarget, shadow Shadow) string {
	return chunksql.Insert(shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), target.PKColumn(), shadow.CopyColumns(), shadow.Conversions())
}

// copySearchPath is the search_path every chunk transaction runs under,
// chosen by whether the copy statement carries conversion expressions.
func copySearchPath(shadow Shadow) string {
	return chunksql.SearchPath(shadow.Schema(), len(shadow.Conversions()) > 0)
}
