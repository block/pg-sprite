package schemachange

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ErrNotDerivedName reports a name that no table in the schema derives, so
// it is not a relation this engine created or retained.
var ErrNotDerivedName = errors.New("name is not derived from any table in the schema")

// namePrefix marks every relation the copy-and-swap route creates or
// retains, so an operator can list the engine's leftovers by prefix.
const namePrefix = "_pgsprite_"

// DerivedKind says which derived name a source table produced.
type DerivedKind string

const (
	// DerivedShadow is the shadow table built for a source table.
	DerivedShadow DerivedKind = "shadow"
	// DerivedOld is the source table retained under its post-cutover name.
	DerivedOld DerivedKind = "old"
)

// nameHash is the sixteen-hex-digit FNV-1a hash of its input.
func nameHash(input string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(input))
	return fmt.Sprintf("%016x", h.Sum64())
}

// NameHash is the sixteen-hex-digit FNV-1a hash of "schema.table" that keys
// every derived copy-and-swap name. The same source table always derives the
// same names, so a resumed run finds the shadow it built earlier.
func NameHash(schema, table string) string {
	return nameHash(schema + "." + table)
}

// ShadowName is the shadow table's name for a source table.
func ShadowName(schema, table string) string {
	return namePrefix + NameHash(schema, table) + "_new"
}

// OldName is the retained source table's name after cutover.
func OldName(schema, table string) string {
	return namePrefix + NameHash(schema, table) + "_old"
}

// OldDependentName is the name an index, extended-statistics object, or
// identity sequence of the retained source table wears after cutover, which
// frees its original name for the shadow's corresponding dependent. The
// dependent is hashed like the table, so every derived name has one fixed
// width well inside PostgreSQL's identifier limit and no source name, however
// long, can push a derived name into server-side truncation.
func OldDependentName(schema, table, dependent string) string {
	return OldName(schema, table) + "_" + nameHash(dependent)
}

// SourceOfDerivedName maps a relation name the engine derived back to the
// table it was derived from: the hash has no inverse, but a derived relation
// always lives in its source's schema, so the source is whichever table in
// that schema derives the name. It returns ErrNotDerivedName when none does.
func SourceOfDerivedName(ctx context.Context, db dbconn.Querier, schema, derived string) (string, DerivedKind, error) {
	rows, err := db.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r'`, schema)
	if err != nil {
		return "", "", fmt.Errorf("list tables in schema %s: %w", schema, err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", "", fmt.Errorf("list tables in schema %s: %w", schema, err)
	}
	for _, table := range tables {
		switch derived {
		case ShadowName(schema, table):
			return table, DerivedShadow, nil
		case OldName(schema, table):
			return table, DerivedOld, nil
		}
	}
	return "", "", fmt.Errorf("%w: %s.%s", ErrNotDerivedName, schema, derived)
}
