package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// tableIdent is the schema-qualified checkpoint table as it appears in
// SQL: SchemaName and TableName quoted, which a unit test pins to what
// pgx.Identifier would render for them. Both names are the engine's own
// constants, never a caller's.
const tableIdent = `"` + SchemaName + `"."` + TableName + `"`

// createSchemaSQL creates the engine schema. It runs only when the schema
// is absent; CREATE SCHEMA checks database-level CREATE before it checks
// for existence, so running it against a pre-provisioned schema would
// refuse an engine role that was deliberately never granted CREATE.
const createSchemaSQL = `CREATE SCHEMA "` + SchemaName + `"`

// ensureLockSQL takes the engine's own advisory key for the checkpoint
// table — the key pkg/dbconn's table lock would derive for it — for the
// rest of the transaction, so two engines reaching the same database for
// the first time at once serialize their creates instead of racing them
// into a duplicate-key failure in the catalog, and so the owner read below
// cannot interleave with another engine's create.
const ensureLockSQL = "SELECT pg_advisory_xact_lock($1, hashtext(quote_ident($2) || '.' || quote_ident($3)))"

// ownersSQL reads who owns the engine schema and whatever wears the
// checkpoint table's name inside it, alongside the role this session runs
// as. No row means no schema; a NULL relation means the schema exists and
// the table does not.
const ownersSQL = `
SELECT current_user, pg_get_userbyid(n.nspowner), c.relkind::text, pg_get_userbyid(c.relowner)
FROM pg_namespace n
LEFT JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = $2
WHERE n.nspname = $1`

// relkindOrdinaryTable is pg_class.relkind for a plain table.
const relkindOrdinaryTable = "r"

// engineObjects is what Ensure found under the engine's names: which of
// the two objects exist, and whether the engine owns them.
type engineObjects struct {
	schemaExists bool
	tableExists  bool
}

// Ensure makes the checkpoint table usable by this engine role and is a
// no-op once it is. It creates only what is absent: a pre-provisioned
// schema and table that the engine owns are accepted as they are, so a
// deployment that keeps database-level CREATE away from the engine role can
// create them for it beforehand. A schema or table that exists under
// another owner, or a relation under the table's name that is not a plain
// table, is refused with ErrForeignObject rather than adopted. A table an
// older engine created is brought up to this engine's shape by adding the
// columns it lacks, so its rows stay readable: that ALTER TABLE takes
// ACCESS EXCLUSIVE on the one table every target's row lives in, waits at
// most the session's lock_timeout for the writes in flight to clear, and
// is safe to call again after a lock timeout, which is returned as the
// server reported it. A table with columns this engine does not know is
// refused with a *TableNewerError (ErrTableNewer), untouched. Every read
// and write before Ensure has evolved an older table reports ErrTableBehind.
func (s *Store) Ensure(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create of %s: %w", tableIdent, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if _, err := tx.Exec(ctx, ensureLockSQL, dbconn.TableLockClassID, SchemaName, TableName); err != nil {
		return fmt.Errorf("lock for create of %s: %w", tableIdent, err)
	}
	found, err := inspectEngineObjects(ctx, tx)
	if err != nil {
		return err
	}
	if !found.schemaExists {
		if _, err := tx.Exec(ctx, createSchemaSQL); err != nil {
			return fmt.Errorf("create schema %q: %w", SchemaName, err)
		}
	}
	if found.tableExists {
		if err := evolveTable(ctx, tx); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, createTableSQL()); err != nil {
		return fmt.Errorf("create %s: %w", tableIdent, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create of %s: %w", tableIdent, err)
	}
	return nil
}

// inspectEngineObjects reads the engine schema and table from the catalog
// and refuses, typed, anything under their names that is not this role's
// own plain table.
func inspectEngineObjects(ctx context.Context, tx pgx.Tx) (engineObjects, error) {
	var self, schemaOwner string
	var relkind, tableOwner *string
	err := tx.QueryRow(ctx, ownersSQL, SchemaName, TableName).Scan(&self, &schemaOwner, &relkind, &tableOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return engineObjects{}, nil
	}
	if err != nil {
		return engineObjects{}, fmt.Errorf("inspect %s: %w", tableIdent, err)
	}
	if schemaOwner != self {
		return engineObjects{}, fmt.Errorf("%w: schema %q is owned by %s, this engine runs as %s", ErrForeignObject, SchemaName, schemaOwner, self)
	}
	if relkind == nil {
		return engineObjects{schemaExists: true}, nil
	}
	if *relkind != relkindOrdinaryTable {
		return engineObjects{}, fmt.Errorf("%w: %s is a relation of kind %q, not a table", ErrForeignObject, tableIdent, *relkind)
	}
	if *tableOwner != self {
		return engineObjects{}, fmt.Errorf("%w: %s is owned by %s, this engine runs as %s", ErrForeignObject, tableIdent, *tableOwner, self)
	}
	return engineObjects{schemaExists: true, tableExists: true}, nil
}
