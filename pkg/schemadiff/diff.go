package schemadiff

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrUnsupportedChange is returned when converging live onto desired would
// need a change the engine does not derive (identity or generation changes
// on an existing column). The caller surfaces it; nothing is guessed.
var ErrUnsupportedChange = errors.New("unsupported schema change")

// ErrDifferentTables is returned when the two models describe different
// tables — a caller bug, refused rather than diffed.
var ErrDifferentTables = errors.New("models describe different tables")

// ChangeKind classifies a derived statement so a consumer can gate whole
// classes of change (destructive, rewriting, index-building) without
// parsing SQL.
type ChangeKind string

// The change kinds a plan can contain.
const (
	// ChangeCreateTable creates the table (missing-table plans only).
	ChangeCreateTable ChangeKind = "create-table"
	// ChangeDropIndex drops an index.
	ChangeDropIndex ChangeKind = "drop-index"
	// ChangeDropConstraint drops a table constraint.
	ChangeDropConstraint ChangeKind = "drop-constraint"
	// ChangeDropColumn drops a column.
	ChangeDropColumn ChangeKind = "drop-column"
	// ChangeAddColumn adds a column.
	ChangeAddColumn ChangeKind = "add-column"
	// ChangeAlterType changes a column's type.
	ChangeAlterType ChangeKind = "alter-type"
	// ChangeSetDefault sets or replaces a column default.
	ChangeSetDefault ChangeKind = "set-default"
	// ChangeDropDefault drops a column default.
	ChangeDropDefault ChangeKind = "drop-default"
	// ChangeSetNotNull adds the NOT NULL attribute.
	ChangeSetNotNull ChangeKind = "set-not-null"
	// ChangeDropNotNull removes the NOT NULL attribute.
	ChangeDropNotNull ChangeKind = "drop-not-null"
	// ChangeAddConstraint adds a table constraint.
	ChangeAddConstraint ChangeKind = "add-constraint"
	// ChangeCreateIndex creates an index.
	ChangeCreateIndex ChangeKind = "create-index"
)

// ChangeKinds returns the closed set of ChangeKind values. It is part of
// the plan-report contract (docs/plan-report.md): the set changes only with
// a format_version bump, and a consumer that meets an unrecognized value
// must treat the statement as unknown and refuse it.
func ChangeKinds() []ChangeKind {
	return []ChangeKind{
		ChangeCreateTable,
		ChangeDropIndex,
		ChangeDropConstraint,
		ChangeDropColumn,
		ChangeAddColumn,
		ChangeAlterType,
		ChangeSetDefault,
		ChangeDropDefault,
		ChangeSetNotNull,
		ChangeDropNotNull,
		ChangeAddConstraint,
		ChangeCreateIndex,
	}
}

// Change is one derived statement of the ordered plan.
type Change struct {
	// SQL is the literal statement, without a trailing semicolon.
	SQL string `json:"sql"`
	// Kind classifies the statement for consumers that gate by class.
	Kind ChangeKind `json:"kind"`
	// Destructive marks statements that discard data, constraints, or
	// indexes (column, constraint, and index drops — dropping a unique
	// index discards the same guarantee as dropping a unique constraint).
	// Destructive changes are gated by the caller, never executed
	// silently.
	Destructive bool `json:"destructive,omitempty"`
}

// Diff derives the ordered statement list that converges live onto desired.
// Order is dependency-correct: drops first (indexes, then constraints, then
// columns), then column adds and alters, then constraint adds, then index
// creates — so an added column exists before an index or constraint that
// references it. Within each bucket the order is deterministic: attribute
// order for columns, name order for constraints and indexes. schema
// qualifies the emitted statements' table references. Columns are compared
// by name only: attribute order carries no semantics in PostgreSQL and is
// deliberately out of scope for convergence.
func Diff(schema string, live, desired Model) ([]Change, error) {
	if live.Table != desired.Table {
		return nil, fmt.Errorf("%w: %q vs %q", ErrDifferentTables, live.Table, desired.Table)
	}
	// Partitioning is table identity, not an alterable attribute: no ALTER
	// can add, remove, or change a partition key or a partition attachment
	// in place, so a mismatch fails closed instead of diffing to silence.
	if live.PartitionKey != desired.PartitionKey {
		return nil, fmt.Errorf("partition key %q vs %q: %w", live.PartitionKey, desired.PartitionKey, ErrUnsupportedChange)
	}
	if live.IsPartition != desired.IsPartition {
		return nil, fmt.Errorf("partition attachment differs between live and desired: %w", ErrUnsupportedChange)
	}
	table := pgx.Identifier{schema, live.Table}.Sanitize()

	liveCols := columnsByName(live.Columns)
	desiredCols := columnsByName(desired.Columns)
	liveCons := constraintsByName(live.Constraints)
	desiredCons := constraintsByName(desired.Constraints)
	liveIdx := indexesByName(live.Indexes)
	desiredIdx := indexesByName(desired.Indexes)

	var changes []Change

	// Indexes to drop: gone from desired, or changed (dropped here,
	// recreated in the create bucket below).
	for _, ix := range live.Indexes {
		want, ok := desiredIdx[ix.Name]
		if !ok || want.Def != ix.Def {
			changes = append(changes, Change{
				SQL:         "DROP INDEX " + pgx.Identifier{schema, ix.Name}.Sanitize(),
				Kind:        ChangeDropIndex,
				Destructive: true,
			})
		}
	}

	// Constraints to drop: gone from desired, or changed (re-added below).
	for _, con := range live.Constraints {
		want, ok := desiredCons[con.Name]
		if !ok || want.Def != con.Def {
			changes = append(changes, Change{
				SQL:         "ALTER TABLE " + table + " DROP CONSTRAINT " + pgx.Identifier{con.Name}.Sanitize(),
				Kind:        ChangeDropConstraint,
				Destructive: true,
			})
		}
	}

	// Columns to drop. A rename is indistinguishable from drop+add at the
	// catalog level, so it surfaces as exactly that — and the drop is
	// flagged destructive for the caller to gate.
	for _, col := range live.Columns {
		if _, ok := desiredCols[col.Name]; !ok {
			changes = append(changes, Change{
				SQL:         "ALTER TABLE " + table + " DROP COLUMN " + pgx.Identifier{col.Name}.Sanitize(),
				Kind:        ChangeDropColumn,
				Destructive: true,
			})
		}
	}

	// Columns to add. A sequence-backed default (serial) cannot be added:
	// the desired-side sequence existed only inside the rolled-back
	// scratch transaction, so the emitted default would reference a
	// relation the plan never creates.
	for _, col := range desired.Columns {
		if _, ok := liveCols[col.Name]; !ok {
			if col.SequenceDefault {
				return nil, fmt.Errorf("%w: column %q has a sequence-backed default (serial); the plan cannot create its sequence", ErrUnsupportedChange, col.Name)
			}
			changes = append(changes, Change{
				SQL:  "ALTER TABLE " + table + " ADD COLUMN " + columnDef(col),
				Kind: ChangeAddColumn,
			})
		}
	}

	// Columns present on both sides: type, default, and nullability deltas.
	for _, col := range desired.Columns {
		liveCol, ok := liveCols[col.Name]
		if !ok {
			continue
		}
		alter, err := alterColumnChanges(table, liveCol, col)
		if err != nil {
			return nil, err
		}
		changes = append(changes, alter...)
	}

	// Constraints to add: new, or re-added after a definition change.
	for _, con := range desired.Constraints {
		had, ok := liveCons[con.Name]
		if !ok || had.Def != con.Def {
			changes = append(changes, Change{
				SQL:  "ALTER TABLE " + table + " ADD CONSTRAINT " + pgx.Identifier{con.Name}.Sanitize() + " " + con.Def,
				Kind: ChangeAddConstraint,
			})
		}
	}

	// Indexes to create: new, or recreated after a definition change. The
	// desired definition is server-decompiled and unqualified; only the
	// schema qualification is injected.
	for _, ix := range desired.Indexes {
		had, ok := liveIdx[ix.Name]
		if !ok || had.Def != ix.Def {
			qualified, err := statement.Qualify(ix.Def, schema)
			if err != nil {
				return nil, fmt.Errorf("qualify index %s: %w", ix.Name, err)
			}
			changes = append(changes, Change{SQL: qualified, Kind: ChangeCreateIndex})
		}
	}

	return changes, nil
}

// alterColumnChanges derives the in-place column alterations between two
// versions of the same column. Identity and generation cannot be altered in
// place, so a delta there is refused as unsupported.
func alterColumnChanges(table string, live, desired Column) ([]Change, error) {
	if live.Identity != desired.Identity {
		return nil, fmt.Errorf("%w: column %q identity change", ErrUnsupportedChange, live.Name)
	}
	if live.Generated != desired.Generated {
		return nil, fmt.Errorf("%w: column %q generated change", ErrUnsupportedChange, live.Name)
	}
	if desired.Generated && (live.Type != desired.Type || live.Default != desired.Default) {
		return nil, fmt.Errorf("%w: column %q generation expression or type change", ErrUnsupportedChange, live.Name)
	}
	col := pgx.Identifier{live.Name}.Sanitize()
	var changes []Change
	if live.Type != desired.Type {
		changes = append(changes, Change{
			SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " TYPE " + desired.Type,
			Kind: ChangeAlterType,
		})
	}
	if !desired.Generated && desired.Identity == IdentityNone && live.Default != desired.Default {
		// A sequence-backed desired default (serial adoption) is refused:
		// the sequence existed only inside the rolled-back scratch
		// transaction, so the emitted SET DEFAULT would reference a
		// relation the plan never creates — or worse, silently bind to an
		// unrelated live sequence of the same name.
		if desired.SequenceDefault {
			return nil, fmt.Errorf("%w: column %q would adopt a sequence-backed default (serial); the plan cannot create its sequence", ErrUnsupportedChange, live.Name)
		}
		if desired.Default == "" {
			changes = append(changes, Change{
				SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " DROP DEFAULT",
				Kind: ChangeDropDefault,
			})
		} else {
			changes = append(changes, Change{
				SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " SET DEFAULT " + desired.Default,
				Kind: ChangeSetDefault,
			})
		}
	}
	if live.NotNull != desired.NotNull {
		if desired.NotNull {
			changes = append(changes, Change{
				SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " SET NOT NULL",
				Kind: ChangeSetNotNull,
			})
		} else {
			changes = append(changes, Change{
				SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " DROP NOT NULL",
				Kind: ChangeDropNotNull,
			})
		}
	}
	return changes, nil
}

// columnDef renders a canonical column definition for ADD COLUMN. All parts
// are server-canonical: the type from format_type, expressions from
// pg_get_expr.
func columnDef(c Column) string {
	def := pgx.Identifier{c.Name}.Sanitize() + " " + c.Type
	switch {
	case c.Generated:
		def += " GENERATED ALWAYS AS (" + c.Default + ") STORED"
	case c.Identity == IdentityAlways:
		def += " GENERATED ALWAYS AS IDENTITY"
	case c.Identity == IdentityByDefault:
		def += " GENERATED BY DEFAULT AS IDENTITY"
	case c.Default != "":
		def += " DEFAULT " + c.Default
	}
	if c.NotNull {
		def += " NOT NULL"
	}
	return def
}

// columnsByName indexes columns for lookup during the diff.
func columnsByName(cols []Column) map[string]Column {
	m := make(map[string]Column, len(cols))
	for _, c := range cols {
		m[c.Name] = c
	}
	return m
}

// constraintsByName indexes constraints for lookup during the diff.
func constraintsByName(cons []Constraint) map[string]Constraint {
	m := make(map[string]Constraint, len(cons))
	for _, c := range cons {
		m[c.Name] = c
	}
	return m
}

// indexesByName indexes indexes for lookup during the diff.
func indexesByName(idxs []Index) map[string]Index {
	m := make(map[string]Index, len(idxs))
	for _, ix := range idxs {
		m[ix.Name] = ix
	}
	return m
}
