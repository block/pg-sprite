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
const writeFenceFunctionSQL = `
CREATE OR REPLACE FUNCTION ` + preflight.EngineSchema + `.` + WriteFenceTrigger + `() RETURNS trigger
LANGUAGE plpgsql AS $pgsprite$
BEGIN
	RAISE EXCEPTION USING
		ERRCODE = '` + WriteFenceSQLState + `',
		MESSAGE = format('%I.%I is quiesced for a copy-and-swap schema change; the write is refused', TG_TABLE_SCHEMA, TG_TABLE_NAME),
		HINT = 'Wait for the schema change to complete, or stop it and drop its write fence.';
END
$pgsprite$`

// writeFenceLockSQL serializes two engines creating the fence function
// for the first time in one database, on the engine's advisory key for
// the function, so neither meets the catalog's duplicate-key failure for
// a CREATE the other has in flight. Every function and operator is
// qualified (INV: CO-9), so a planted overload in a searched schema cannot
// change the key even if the transaction's catalog-only search_path were
// not set.
const writeFenceLockSQL = "SELECT pg_catalog.pg_advisory_xact_lock($1, pg_catalog.hashtext(" +
	"pg_catalog.quote_ident($2) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident($3)))"

// writeFenceTriggerType is pg_trigger.tgtype for the fence as installed: a
// statement-level BEFORE trigger on INSERT (4), DELETE (8), UPDATE (16), and
// TRUNCATE (32), the BEFORE bit being 2 and the row-level bit unset.
const writeFenceTriggerType = 2 + 4 + 8 + 16 + 32

// triggerEnabledAlways is pg_trigger.tgenabled for a trigger that fires in
// every session, whatever its session_replication_role.
const triggerEnabledAlways = "A"

// writeFenceStateSQL reads the enablement of the engine's fence on one
// relation: the row exists only for a trigger of the fence's name, calling
// the fence's function in the engine's schema, of the shape the install
// gives it — a statement-level BEFORE trigger on every write statement
// (tgtype), with no WHEN condition (tgqual), no UPDATE OF column list
// (tgattr), no arguments (tgnargs), and not deferrable. A trigger of the
// same name and function whose shape differs fires for fewer statements
// than the fence, so it yields no row and the swap refuses it as a fence
// that is gone. Every catalog name is qualified, so the read does not
// depend on the session's search_path (CO-9).
const writeFenceStateSQL = `
SELECT tg.tgenabled::pg_catalog.text
FROM pg_catalog.pg_trigger tg
JOIN pg_catalog.pg_proc p ON p.oid OPERATOR(pg_catalog.=) tg.tgfoid
JOIN pg_catalog.pg_namespace pn ON pn.oid OPERATOR(pg_catalog.=) p.pronamespace
WHERE tg.tgrelid OPERATOR(pg_catalog.=) $1::pg_catalog.oid
  AND tg.tgname::pg_catalog.text OPERATOR(pg_catalog.=) $2::pg_catalog.text
  AND p.proname::pg_catalog.text OPERATOR(pg_catalog.=) $2::pg_catalog.text
  AND pn.nspname::pg_catalog.text OPERATOR(pg_catalog.=) $3::pg_catalog.text
  AND tg.tgtype::pg_catalog.int4 OPERATOR(pg_catalog.=) $4::pg_catalog.int4
  AND tg.tgqual IS NULL
  AND tg.tgattr::pg_catalog.text OPERATOR(pg_catalog.=) ''
  AND tg.tgnargs::pg_catalog.int4 OPERATOR(pg_catalog.=) 0
  AND NOT tg.tgdeferrable
  AND NOT tg.tgisinternal`

// installWriteFence puts the write fence on the source, in one bounded
// transaction as the engine role and only while the caller holds the
// table lock. The trigger is a statement-level BEFORE trigger on INSERT,
// UPDATE, DELETE, and TRUNCATE, so it fires once per statement and costs
// the run nothing per row; installing it waits, under lock_timeout, for
// any write already in flight to finish, which is the point at which the
// table is quiesced. The trigger is enabled ALWAYS, so it also fires for a
// session running under session_replication_role = replica, which skips
// every ordinary trigger. The install is idempotent, so a resumed run
// installs over the fence the stopped run left, and a fence that run left
// disabled, or enabled for origin sessions only, comes back enabled
// ALWAYS. The trigger is created as the engine role, not the owner: the
// function lives in the engine's schema, and the owner needs no access to
// that schema for the trigger to fire.
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
	if err := pinCatalogSearchPath(ctx, tx); err != nil {
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
	// INV: CO-1 — a session in replica role fires no ORIGIN trigger, so the
	// fence must be ALWAYS to refuse its writes too.
	enable := "ALTER TABLE " + source + " ENABLE ALWAYS TRIGGER " + pgx.Identifier{WriteFenceTrigger}.Sanitize()
	if _, err := tx.Exec(ctx, enable); err != nil {
		return fmt.Errorf("enable write fence always on %s.%s: %w", target.Schema(), target.Table(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit write fence install: %w", err)
	}
	return nil
}

// confirmWriteFence refuses the swap of a quiesced proof unless the source
// still carries the engine's fence, of the shape the install gave it and
// enabled ALWAYS. It runs under the swap's ACCESS EXCLUSIVE lock, so the
// trigger cannot be disabled, dropped, or replaced between this read and
// the rename. The fence is what makes the proof's quiescence true rather
// than assumed; a source whose fence was disabled, dropped, or replaced by
// a trigger that fires for fewer statements since the pass may have taken
// writes the pass never saw, and the run stops there for a fresh install
// and a fresh pass.
func confirmWriteFence(ctx context.Context, tx pgx.Tx, built BuiltShadow) error {
	schema, table := built.Schema(), built.SourceTable()
	var enabled string
	err := tx.QueryRow(ctx, writeFenceStateSQL, built.SourceOID(), WriteFenceTrigger, preflight.EngineSchema, writeFenceTriggerType).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		// INV: CO-1
		return refuse(CauseCutoverUnfenced, nil, "source %s.%s carries no %s trigger of the fence's shape calling %s.%s, so the quiesced proof no longer holds",
			schema, table, WriteFenceTrigger, preflight.EngineSchema, WriteFenceTrigger)
	}
	if err != nil {
		return fmt.Errorf("read write fence on %s.%s: %w", schema, table, err)
	}
	if enabled != triggerEnabledAlways {
		// INV: CO-1
		return refuse(CauseCutoverUnfenced, nil, "the write fence on %s.%s has tgenabled %q, not %q, so a write could pass it",
			schema, table, enabled, triggerEnabledAlways)
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
// (ST-5). A source without the fence is left as it is. The answer does not
// depend on the pool's session search_path: the drop's transaction searches the
// catalog alone, so a caller-built pool with another schema ahead of it
// gets the same verdict.
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
	if err := pinCatalogSearchPath(ctx, tx); err != nil {
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

// pinCatalogSearchPath scopes a fence transaction's search_path to
// pg_catalog alone. The transaction runs on the caller's pool, under
// whatever session search_path that pool was built with, and not every
// read in it is this package's own: the table lock confirmation reads
// pg_locks through pkg/dbconn. With the catalog the only searched schema,
// no planted relation, function, or operator can answer any of them
// (CO-9). The fence's own statements name every object by schema, so
// they need no path; the pin and their qualification are two layers for
// the same guarantee, and neither is the redundant one.
func pinCatalogSearchPath(ctx context.Context, tx pgx.Tx) error {
	// INV: CO-9
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return fmt.Errorf("set write fence search_path: %w", err)
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
	// INV: CO-9 — every catalog name is qualified, so the read does not
	// depend on the transaction's search_path and a planted overload in a
	// searched schema cannot answer it.
	err := tx.QueryRow(ctx, `
		SELECT c.relkind::pg_catalog.text, pg_catalog.pg_get_userbyid(c.relowner)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`, schema, table).Scan(&relkind, &owner)
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
