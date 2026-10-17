package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// WriteFenceTrigger is the name of the statement trigger a quiesced run
// installs on its source so that a write reaching the table while the
// run is in progress is refused by the server rather than lost. It is
// also the name of the function the trigger calls, in the engine's own
// schema; the preflight shape check sets that trigger aside when it counts
// the table's user triggers.
const WriteFenceTrigger = "pgsprite_write_fence"

// WriteFenceSQLState is the SQLSTATE a write refused by the fence carries:
// object_in_use, the class of error a writer gets when a relation is
// busy with something else.
const WriteFenceSQLState = "55006"

// ErrSourceNotFound reports that no relation bears the source's name.
var ErrSourceNotFound = errors.New("source table not found")

// writeFenceFunctionSQL creates or replaces the fence's function. The
// function refuses every statement that calls it, naming the table from
// the trigger context so the same function serves every fenced table.
// It is created once per database, as the engine role, and owned by it.
// The body runs in the writer's session under whatever search_path the
// writer has, so every name in it is qualified with pg_catalog.
// INV: CO-9
const writeFenceFunctionSQL = `
CREATE OR REPLACE FUNCTION ` + preflight.EngineSchema + `.` + WriteFenceTrigger + `() RETURNS trigger
LANGUAGE plpgsql AS $pgsprite$
BEGIN
	RAISE EXCEPTION USING
		ERRCODE = '` + WriteFenceSQLState + `',
		MESSAGE = pg_catalog.format('%I.%I is quiesced for a copy-and-swap schema change; the write is refused', TG_TABLE_SCHEMA, TG_TABLE_NAME),
		HINT = 'Wait for the schema change to complete, or stop it and drop its write fence.';
END
$pgsprite$`

// writeFenceLockSQL serializes two engines creating the fence function
// for the first time in one database, on the engine's advisory key for
// the function, so neither meets the catalog's duplicate-key failure for
// a CREATE the other has in flight.
const writeFenceLockSQL = "SELECT pg_advisory_xact_lock($1, hashtext(quote_ident($2) || '.' || quote_ident($3)))"

// installWriteFence puts the write fence on the source, in one bounded
// transaction as the engine role and only while the caller holds the
// table lock. The trigger is a statement-level BEFORE trigger on INSERT,
// UPDATE, DELETE, and TRUNCATE, so it fires once per statement and costs
// the run nothing per row; installing it waits, under lock_timeout, for
// any write already in flight to finish, which is the point at which the
// table is quiesced. The install is idempotent, so a resumed run installs
// over the fence the stopped run left. The trigger is created as the
// engine role, not the owner: the function lives in the engine's schema,
// and the owner needs no access to that schema for the trigger to fire.
func installWriteFence(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, opts Options) error {
	if err := requireTableLock(lock, target.Schema(), target.Table()); err != nil {
		return err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin write fence install: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBudgets(ctx, tx, opts); err != nil {
		return err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return err
	}
	if err := checkSource(ctx, tx, target); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, writeFenceLockSQL, dbconn.TableLockClassID, preflight.EngineSchema, WriteFenceTrigger); err != nil {
		return fmt.Errorf("lock for create of the write fence function: %w", err)
	}
	if _, err := tx.Exec(ctx, writeFenceFunctionSQL); err != nil {
		return fmt.Errorf("create write fence function %s.%s: %w", preflight.EngineSchema, WriteFenceTrigger, err)
	}
	source := pgx.Identifier{target.Schema(), target.Table()}.Sanitize()
	function := pgx.Identifier{preflight.EngineSchema, WriteFenceTrigger}.Sanitize()
	create := "CREATE OR REPLACE TRIGGER " + pgx.Identifier{WriteFenceTrigger}.Sanitize() +
		" BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON " + source +
		" FOR EACH STATEMENT EXECUTE FUNCTION " + function + "()"
	if _, err := tx.Exec(ctx, create); err != nil {
		return fmt.Errorf("install write fence on %s.%s: %w", target.Schema(), target.Table(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit write fence install: %w", err)
	}
	return nil
}

// DropWriteFence takes the write fence off the source of a run that is
// being abandoned, so the table takes writes again. A run that completes
// needs no call: the fence goes to the retained source with the swap and
// is dropped with it, or taken off it under KeepOld. The drop runs in one
// bounded transaction and only while the caller holds the table lock, so
// it cannot race a run on the same table, and it refuses anything under
// the source's name that is not a plain table owned by the proof's owner
// (ST-5). A source without the fence is left as it is.
func DropWriteFence(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if err := checkProof(target); err != nil {
		return err
	}
	if err := requireTableLock(lock, target.Schema(), target.Table()); err != nil {
		return err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	err := dropWriteFence(ctx, pool, lock, opts, func(ctx context.Context, tx pgx.Tx) error {
		return checkSource(ctx, tx, target)
	}, target.Schema(), target.Table())
	if err != nil {
		return lockLossCause(lock, err)
	}
	return nil
}

// dropWriteFenceFromOld takes the write fence off the retained source
// after a swap the caller keeps the old table through: the fence rode the
// rename to the old name, and a table kept for a rollback window must be
// writable again the moment it is renamed back. Only the relation the swap
// proof recorded as the source is touched (ST-6).
func dropWriteFenceFromOld(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, swapped SwappedTable, opts Options) error {
	if err := requireTableLock(lock, swapped.Schema(), swapped.Table()); err != nil {
		return err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	schema, old := swapped.Schema(), swapped.OldTable()
	err := dropWriteFence(ctx, pool, lock, opts, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockOldTable(ctx, tx, schema, old); err != nil {
			return err
		}
		oid, err := resolveRelation(ctx, tx, schema, old)
		if err != nil {
			return err
		}
		if oid != swapped.OldOID() {
			// INV: ST-6
			return refuse(CauseRelationReplaced, nil, "old table %s.%s is relation %d, the swap retained %d", schema, old, oid, swapped.OldOID())
		}
		return nil
	}, schema, old)
	if err != nil {
		return lockLossCause(lock, err)
	}
	return nil
}

// dropWriteFence drops the fence trigger from schema.table once prove has
// confirmed, inside the transaction, that the relation under the name is
// the one the caller means.
func dropWriteFence(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, opts Options, prove func(context.Context, pgx.Tx) error, schema, table string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin write fence drop: %w", err)
	}
	defer func() {
		// Redundant safety closer: see installWriteFence.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBudgets(ctx, tx, opts); err != nil {
		return err
	}
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return err
	}
	if err := prove(ctx, tx); err != nil {
		return err
	}
	drop := "DROP TRIGGER IF EXISTS " + pgx.Identifier{WriteFenceTrigger}.Sanitize() + " ON " + pgx.Identifier{schema, table}.Sanitize()
	if _, err := tx.Exec(ctx, drop); err != nil {
		return fmt.Errorf("drop write fence from %s.%s: %w", schema, table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit write fence drop: %w", err)
	}
	return nil
}

// checkSource confirms that the source's name is borne by a plain table
// owned by the proof's owner. Nothing under the name is ErrSourceNotFound;
// another kind of relation, or a table of another owner, is refused as a
// foreign relation (ST-5), since the proof was minted for neither.
func checkSource(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget) error {
	schema, table := target.Schema(), target.Table()
	var relkind, owner string
	err := tx.QueryRow(ctx, `
		SELECT c.relkind::text, pg_get_userbyid(c.relowner)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&relkind, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s.%s", ErrSourceNotFound, schema, table)
	}
	if err != nil {
		return fmt.Errorf("resolve source %s.%s: %w", schema, table, err)
	}
	if relkind != relkindOrdinaryTable {
		// INV: ST-5
		return refuse(CauseForeignRelation, nil, "%s.%s is a relation of kind %q, not a table", schema, table, relkind)
	}
	if owner != target.OwnerRole() {
		// INV: ST-5
		return refuse(CauseForeignRelation, nil, "source %s.%s is owned by %s, the proof says %s", schema, table, owner, target.OwnerRole())
	}
	return nil
}
