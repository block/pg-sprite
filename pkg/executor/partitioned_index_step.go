package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// onlyIndexTable is the identity a metadata-only index is created against: a
// partitioned table, by OID, with the schema and name the statement resolves.
type onlyIndexTable struct {
	oid    uint32
	schema string
	name   string
}

func (t onlyIndexTable) qualified() string { return qualifiedName(t.schema, t.name) }

// lockAndProveTable takes a bounded lock on the table the statement will name
// and proves, under it, that the name still answers to the table's identity. A
// statement names a table and the server resolves the name when it runs, so a
// table renamed away and replaced under the same name would otherwise receive
// the index. The lock is the weakest one that excludes a rename, taken under the
// transaction's own lock_timeout (LK-2), and held until the transaction ends.
// INV: ST-7 — the statement runs against the table that was proven.
func lockAndProveTable(ctx context.Context, tx pgx.Tx, table onlyIndexTable) error {
	if err := lockRelationShared(ctx, tx, table.schema, table.name, table.qualified()); err != nil {
		return err
	}
	var proven bool
	if err := tx.QueryRow(ctx, provePartitionedTableSQL, table.oid, table.schema, table.name).Scan(&proven); err != nil {
		return fmt.Errorf("prove table %s: %w", table.qualified(), err)
	}
	if !proven {
		return fmt.Errorf("%w: ST-7: %s is not the partitioned table (%d) the build resolved: it was renamed or replaced since",
			ErrInvariantViolation, table.qualified(), table.oid)
	}
	return nil
}

// lockRelationShared locks a relation by name in SHARE UPDATE EXCLUSIVE mode,
// the weakest mode that excludes a rename and a detach, and which blocks no
// writer. A name that no longer exists is an ST-7 refusal.
func lockRelationShared(ctx context.Context, tx pgx.Tx, schema, name, display string) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{schema, name}.Sanitize()+" IN SHARE UPDATE EXCLUSIVE MODE")
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
		return fmt.Errorf("%w: ST-7: %s no longer exists under that name; it was renamed or dropped since the build resolved it",
			ErrInvariantViolation, display)
	}
	return err
}

const provePartitionedTableSQL = `SELECT EXISTS (
		   SELECT 1
		     FROM pg_catalog.pg_class c
		     JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		    WHERE c.oid OPERATOR(pg_catalog.=) $1
		      AND n.nspname OPERATOR(pg_catalog.=) $2
		      AND c.relname OPERATOR(pg_catalog.=) $3
		      AND c.relkind OPERATOR(pg_catalog.=) 'p')`

// createOnlyIndex runs a CREATE INDEX ... ON ONLY statement in tx on a table it
// has locked and proved, and returns the index it created, read under the
// catalog-only search_path. The index is checked by identity before the
// transaction can commit: it must be a partitioned index on the proved table
// (INV: ST-7), and any other outcome fails the step, which rolls back.
func createOnlyIndex(ctx context.Context, tx pgx.Tx, table onlyIndexTable, indexSchema, indexName, createSQL string) (relationFacts, error) {
	if err := lockAndProveTable(ctx, tx, table); err != nil {
		return relationFacts{}, err
	}
	if _, err := tx.Exec(ctx, createSQL); err != nil {
		return relationFacts{}, err
	}
	// The definition is printed under the catalog-only path, the one every
	// later read of the index uses.
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(catalogReadPath()...)); err != nil {
		return relationFacts{}, fmt.Errorf("scope the search path to read the new index: %w", err)
	}
	created, found, err := relationByName(ctx, tx, indexSchema, indexName)
	if err != nil {
		return relationFacts{}, err
	}
	if !found {
		return relationFacts{}, fmt.Errorf("%w: index %s is not in the catalog after it was created",
			ErrInvariantViolation, qualifiedName(indexSchema, indexName))
	}
	if !created.isPartitionedIndex() || created.tableOID != table.oid {
		return relationFacts{}, fmt.Errorf("%w: ST-7: index %s was created on %s, not on %s",
			ErrInvariantViolation, qualifiedName(indexSchema, indexName), describeRelation(created), table.qualified())
	}
	return created, nil
}
