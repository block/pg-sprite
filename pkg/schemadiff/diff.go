package schemadiff

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrUnsupportedChange is returned when converging live onto desired would
// need a change the engine does not derive: identity, generation, or
// collation changes on an existing column, a persistence (unlogged) or
// storage-parameter difference, or a partitioning difference (partition key or partition
// attachment) — table identity that no ALTER converges. The caller
// surfaces it; nothing is guessed. Diff returns it as an
// *UnsupportedChangeError that names the cause.
var ErrUnsupportedChange = errors.New("unsupported schema change")

// ErrDifferentTables is returned when the two models describe different
// tables — a caller bug, refused rather than diffed.
var ErrDifferentTables = errors.New("models describe different tables")

// ErrDifferentTypes is returned when a live enum type and a desired one
// name different types — a caller bug, refused rather than diffed.
var ErrDifferentTypes = errors.New("enum types describe different types")

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
	// ChangeDropConstraint drops a table constraint other than a foreign
	// key.
	ChangeDropConstraint ChangeKind = "drop-constraint"
	// ChangeDropForeignKey drops a foreign-key constraint. Desired-state
	// execution refuses it: a foreign-key drop is planned, not run.
	ChangeDropForeignKey ChangeKind = "drop-foreign-key"
	// ChangeDropColumn drops a column.
	ChangeDropColumn ChangeKind = "drop-column"
	// ChangeAddColumn adds a column.
	ChangeAddColumn ChangeKind = "add-column"
	// ChangeAlterType changes a column's type.
	ChangeAlterType ChangeKind = "alter-type"
	// ChangeSetGeneratedExpression replaces a stored generated column's
	// expression, which recomputes every row under an exclusive lock. The
	// planner has no online path for it, so the statement is always
	// refused: the plan shows the drift and nothing runs it.
	ChangeSetGeneratedExpression ChangeKind = "set-generated-expression"
	// ChangeSetDefault sets or replaces a column default.
	ChangeSetDefault ChangeKind = "set-default"
	// ChangeDropDefault drops a column default.
	ChangeDropDefault ChangeKind = "drop-default"
	// ChangeSetNotNull adds the NOT NULL attribute.
	ChangeSetNotNull ChangeKind = "set-not-null"
	// ChangeDropNotNull removes the NOT NULL attribute.
	ChangeDropNotNull ChangeKind = "drop-not-null"
	// ChangeAddConstraint adds a table constraint other than a foreign key.
	ChangeAddConstraint ChangeKind = "add-constraint"
	// ChangeAddForeignKey adds a foreign-key constraint. The planner runs it
	// as ADD ... NOT VALID followed by VALIDATE CONSTRAINT.
	ChangeAddForeignKey ChangeKind = "add-foreign-key"
	// ChangeValidateConstraint validates a live check constraint left NOT
	// VALID, the state an interrupted online add leaves behind.
	ChangeValidateConstraint ChangeKind = "validate-constraint"
	// ChangeValidateForeignKey validates a live foreign key left NOT VALID.
	ChangeValidateForeignKey ChangeKind = "validate-foreign-key"
	// ChangeCreateIndex creates an index.
	ChangeCreateIndex ChangeKind = "create-index"
	// ChangeRenameIndex renames an index: a replacement built under its
	// temporary name to the name of the index it replaces, or a live index
	// the desired table declares under another name and otherwise unchanged.
	ChangeRenameIndex ChangeKind = "rename-index"
	// ChangeAddEnumValue adds a label to an enum type (ALTER TYPE ... ADD
	// VALUE), the one change a desired enum type file plans.
	ChangeAddEnumValue ChangeKind = "add-enum-value"
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
		ChangeDropForeignKey,
		ChangeDropColumn,
		ChangeAddColumn,
		ChangeAlterType,
		ChangeSetGeneratedExpression,
		ChangeSetDefault,
		ChangeDropDefault,
		ChangeSetNotNull,
		ChangeDropNotNull,
		ChangeAddConstraint,
		ChangeAddForeignKey,
		ChangeValidateConstraint,
		ChangeValidateForeignKey,
		ChangeCreateIndex,
		ChangeRenameIndex,
		ChangeAddEnumValue,
	}
}

// IsForeignKey reports whether the kind adds, drops, or validates a foreign
// key.
func (k ChangeKind) IsForeignKey() bool {
	return k == ChangeAddForeignKey || k == ChangeDropForeignKey || k == ChangeValidateForeignKey
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
	// Adopts is set on an ADD CONSTRAINT ... USING INDEX: the live index
	// the constraint takes over, with the definition the diff matched it
	// against. The statement names only the index, so execution checks
	// the definition again under the table's lock before attaching it.
	// The SQL already identifies the change, so it stays out of the JSON.
	Adopts *IndexAdoption `json:"-"`
	// Validates is set on a VALIDATE CONSTRAINT: the live constraint it
	// validates, with the definition the diff matched it against. The
	// statement names only the constraint, so execution checks the
	// definition again under the table's lock before validating it.
	Validates *ConstraintValidation `json:"-"`
	// AddsNotValid is set on an ADD CONSTRAINT of a CHECK or FOREIGN KEY:
	// the constraint it adds, and the definition introspection reads for it
	// once it is added NOT VALID. A plan that runs the add as ADD ... NOT VALID and then VALIDATE
	// CONSTRAINT checks that definition before the validation, as it does
	// for Validates, so a validation that runs apart from its add never
	// validates a constraint someone put under the same name in between.
	AddsNotValid *ConstraintValidation `json:"-"`
	// References is set on an ADD CONSTRAINT of a FOREIGN KEY: the table
	// and columns the key references, on which the role that runs the plan
	// needs REFERENCES.
	References *ForeignKeyReference `json:"-"`
	// Drops is set on a DROP INDEX, DROP CONSTRAINT, DROP COLUMN, or
	// DROP NOT NULL: the live object the statement removes, with the
	// definition the diff read. A reviewer approves what the name stood for,
	// and execution refuses when the name now stands for something else.
	Drops *DroppedObject `json:"-"`
	// ReplacedBy is set on the DROP INDEX of a changed index the plan
	// replaces, and on the DROP CONSTRAINT of a unique constraint the plan
	// replaces with a standalone index: the index an earlier statement built
	// under the desired definition, which is valid before the drop runs and
	// takes the dropped object's name after it. The table has an index with the
	// desired definition before the old one goes; what the desired
	// definition drops from the old one, the drop removes.
	ReplacedBy string `json:"-"`
	// Replacement is set with ReplacedBy: the index the drop's replacement
	// must still be when the drop runs. The old index is dropped only while
	// it is.
	Replacement *IndexReplacement `json:"-"`
	// Renames is set on a ChangeRenameIndex: the live index the rename
	// gives the desired name and the definition the plan matched it by.
	// The rename runs only while the index still has that definition.
	Renames *IndexRename `json:"-"`
}

// DroppedKind names the kind of object a destructive change removes.
type DroppedKind string

// The kinds of object a destructive change removes.
const (
	// DroppedIndex is an index dropped by DROP INDEX.
	DroppedIndex DroppedKind = "index"
	// DroppedConstraint is a constraint dropped by ALTER TABLE ... DROP
	// CONSTRAINT: its definition names its kind.
	DroppedConstraint DroppedKind = "constraint"
	// DroppedColumn is a column dropped by ALTER TABLE ... DROP COLUMN.
	DroppedColumn DroppedKind = "column"
	// DroppedNotNull is the NOT NULL of a column, removed by ALTER TABLE ...
	// ALTER COLUMN ... DROP NOT NULL.
	DroppedNotNull DroppedKind = "not-null"
)

// DroppedKinds returns the closed set of DroppedKind values. It is part of
// the plan-report contract (docs/plan-report.md).
func DroppedKinds() []DroppedKind {
	return []DroppedKind{DroppedIndex, DroppedConstraint, DroppedColumn, DroppedNotNull}
}

// DroppedObject is the live object a destructive change removes and the
// definition it had when the diff read it: the server's own CREATE INDEX or
// constraint definition, or for a column or a NOT NULL the column's
// attribute number and attributes (see [Column.DroppedDefinition]), which
// differ between a column and one dropped and added again under its name.
type DroppedObject struct {
	// Kind is the kind of object removed.
	Kind DroppedKind `json:"kind"`
	// Name is the object's name: the index, the constraint, or the column.
	Name string `json:"name"`
	// Definition is what the object was when the diff read it.
	Definition string `json:"definition"`
	// Dependents, for a column, are the objects that go with it when it is
	// dropped, rendered by [Dependent.String] in a stable order. Execution
	// refuses a drop that would take along anything the plan did not list;
	// an entry the table has lost since is no reason to refuse.
	Dependents []string `json:"dependents,omitempty"`
	// Replacement, for a constraint, is the index that replaces it, which
	// the run attaches from the plan's [Change.Replacement]. The constraint
	// is dropped only while that index is still in place.
	Replacement *IndexReplacement `json:"-"`
}

// Dependent is an object that goes with a column when the column is dropped.
type Dependent struct {
	// Kind is "index", "constraint", "owned sequence", "identity sequence",
	// or "other".
	Kind string
	// Name is the object's name, or the server's description of an "other".
	Name string
	// Def is the object's definition as the server prints it, which tells
	// an object from another that took its name; empty for "other".
	Def string
	// Referencing reports a foreign key of another table that references
	// the column. Dropping the column takes it along; dropping the table
	// does not, and the server refuses the drop while it exists.
	Referencing bool
}

// String renders the dependent as it appears in [DroppedObject.Dependents].
func (d Dependent) String() string {
	if d.Def == "" {
		return d.Kind + " " + d.Name
	}
	return d.Kind + " " + d.Name + ": " + d.Def
}

// DependentLines renders each dependent for [DroppedObject.Dependents], nil
// when there are none.
func DependentLines(deps []Dependent) []string {
	if len(deps) == 0 {
		return nil
	}
	lines := make([]string, len(deps))
	for i, d := range deps {
		lines[i] = d.String()
	}
	return lines
}

// UnlistedDependents returns the entries of live that planned does not list,
// in live's order.
func UnlistedDependents(planned, live []string) []string {
	var extra []string
	for _, l := range live {
		if !slices.Contains(planned, l) {
			extra = append(extra, l)
		}
	}
	return extra
}

// DroppedDefinition renders the definition a DroppedColumn carries: the
// attribute number, which a column dropped and added again under its name
// never keeps, then type, default or generation or identity, and NOT NULL.
// It reads only fields a catalog query returns for the column alone, so
// execution renders the live column the same way.
func (c Column) DroppedDefinition() string {
	def := fmt.Sprintf("attnum %d: %s", c.AttNum, c.Type)
	if c.Collation != "" {
		def += " COLLATE " + c.Collation
	}
	switch {
	case c.Generated:
		def += " GENERATED ALWAYS AS (" + c.Default + ") STORED"
	case c.Virtual:
		def += " GENERATED ALWAYS AS (" + c.Default + ") VIRTUAL"
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

// DroppedNotNullDefinition renders the definition a DroppedNotNull carries:
// the attribute number alone, since an earlier statement of the same plan
// may change the column's type and default before this one runs.
func (c Column) DroppedNotNullDefinition() string {
	return fmt.Sprintf("attnum %d: NOT NULL", c.AttNum)
}

// IndexAdoption is an index a new primary key or unique constraint takes
// over, and the definition, as introspection reads it (pg_get_indexdef with
// the schema qualification removed), that the index must still have when
// it does.
type IndexAdoption struct {
	Index string
	Def   string
}

// IndexRename is the live index a planned ALTER INDEX ... RENAME TO
// renames and its definition as introspection reads it (pg_get_indexdef
// with the schema qualification removed). An index replaced since planning
// under the same name, such as by a non-unique one, would otherwise take
// the desired unique index's name.
type IndexRename struct {
	Index string
	Def   string
}

// IndexReplacement is the index a changed index's replacement built under
// its temporary name, and the definition, as introspection reads it
// (pg_get_indexdef with the schema qualification removed), that it must
// still have when the old index is dropped. A replacement that is gone,
// invalid, or rebuilt with another definition by then would leave the table
// without the desired index once the old one goes.
type IndexReplacement struct {
	Index string `json:"index"`
	Def   string `json:"def"`
}

// ConstraintValidation is a NOT VALID constraint a planned VALIDATE
// CONSTRAINT validates, and the definition, as introspection reads it
// (pg_get_constraintdef, NOT VALID suffix included), that the constraint
// must still have when it does.
type ConstraintValidation struct {
	Constraint string
	Def        string
}

// Diff derives the ordered statement list that converges live onto desired.
// Order is dependency-correct: drops first (indexes, then constraints, then
// columns, then NOT NULLs), then column adds and alters, then constraint
// adds, then index creates, then foreign key adds — so an added column
// exists before an index or constraint that references it. Four kinds of
// change keep the old object in place until its replacement is, unless the
// plan also drops or retypes a column:
//
//   - A changed index is replaced in the create bucket, built under a
//     temporary name before the old index is dropped (see replacedIndexes).
//   - An index whose name alone changes is renamed in the create bucket
//     with ALTER INDEX ... RENAME TO, and never dropped (see
//     renamedIndexes).
//   - A unique index or unique constraint the plan drops, when the plan adds
//     a unique index or constraint, and a constraint the file declares
//     again under another name, are dropped after the index creates (see
//     heldDrops).
//   - Such a foreign key is dropped after the foreign key adds.
//
// Within each bucket the order is deterministic: attribute order for
// columns, name order for constraints and indexes. schema
// qualifies the emitted statements' table references. Columns are compared
// by name only: attribute order carries no semantics in PostgreSQL and is
// deliberately out of scope for convergence. An invalid index on a plain
// table never yields a drop (see indexNeedsDrop); [Advisories] over the same
// models reports each such entry so the observation is not lost.
// RowSecurity is observational only: table-only desired files do not claim
// ownership of access control, so missing policies never produce policy drops.
func Diff(schema string, live, desired Model) ([]Change, error) {
	if live.Table != desired.Table {
		return nil, fmt.Errorf("%w: %q vs %q", ErrDifferentTables, live.Table, desired.Table)
	}
	if desired.RowSecurity.present() {
		return nil, unsupported(CauseRowSecurity, "desired row security is not managed: %w", ErrUnsupportedChange)
	}
	desired, err := matchGeneratedCheckNames(live, desired)
	if err != nil {
		return nil, err
	}
	desired = adoptReplayedText(live, desired)
	// Partitioning is table identity, not an alterable attribute: no ALTER
	// can add, remove, or change a partition key or a partition attachment
	// in place, so a mismatch fails closed instead of diffing to silence.
	if live.PartitionKey != desired.PartitionKey {
		return nil, unsupported(CausePartitionKey, "partition key %q vs %q: %w", live.PartitionKey, desired.PartitionKey, ErrUnsupportedChange)
	}
	if live.IsPartition != desired.IsPartition {
		return nil, unsupported(CausePartitionAttachment, "partition attachment differs between live and desired: %w", ErrUnsupportedChange)
	}
	// Persistence is convergeable only by a full table rewrite (SET LOGGED
	// / SET UNLOGGED), which the engine does not derive — a mismatch fails
	// closed instead of diffing to silence.
	if live.Unlogged != desired.Unlogged {
		return nil, unsupported(CausePersistence, "table persistence (unlogged) differs between live and desired: %w", ErrUnsupportedChange)
	}
	// Storage parameters are not converged: a mismatch fails closed, so a
	// parameter the file declares is never reported as applied and a tuned
	// live parameter the file omits is never reported as matching.
	if !slices.Equal(live.Options, desired.Options) {
		return nil, unsupported(CauseStorageParameters, "table storage parameters differ between live (%s) and desired (%s); declare the live parameters in the desired file: %w",
			formatOptions(live.Options), formatOptions(desired.Options), ErrUnsupportedChange)
	}
	table := pgx.Identifier{schema, live.Table}.Sanitize()

	liveCols := columnsByName(live.Columns)
	desiredCols := columnsByName(desired.Columns)
	liveCons := constraintsByName(live.Constraints)
	desiredCons := constraintsByName(desired.Constraints)
	liveIdx := indexesByName(live.Indexes)
	desiredIdx := indexesByName(desired.Indexes)

	var changes []Change

	// On a partitioned parent an invalid index means not every partition
	// has a matching attached index. One whose definition matches the desired
	// one is an unfinished partition-by-partition build: the plan carries the
	// same CREATE INDEX, which the partitioned index build resumes. Any other
	// entry is compared by name and definition alone, and the diff never
	// treats a parent's entry as a concurrent build to leave undropped.
	partitioned := live.PartitionKey != ""

	adopted := adoptableIndexes(partitioned, liveCons, liveIdx, desired.Constraints)
	dropsOrRetypes := dropsOrRetypesColumn(live, desiredCols)
	replaced, err := replacedInPlace(partitioned, dropsOrRetypes, schema, liveCons, liveIdx, desired, desiredIdx)
	if err != nil {
		return nil, err
	}
	replacedIdx, err := replacedIndexes(partitioned, dropsOrRetypes, schema, liveCons, liveIdx, desired, desiredIdx, adopted)
	if err != nil {
		return nil, err
	}
	// claimed are the live indexes a replacement accounts for: an earlier
	// run's build it picks up, or a standalone index a unique constraint
	// replaces. None of them is dropped or renamed on its own.
	claimed := make(map[string]struct{}, len(replacedIdx))
	for _, r := range replacedIdx {
		claimed[r.temp] = struct{}{}
	}
	for name, r := range replaced {
		if r.built {
			claimed[replacementIndexName(name)] = struct{}{}
		}
		if r.index.Name != "" {
			claimed[r.index.Name] = struct{}{}
		}
	}
	renamed, err := renamedIndexes(partitioned, live.Indexes, desired.Indexes, liveIdx, desiredIdx, adopted, claimed, replacedIdx)
	if err != nil {
		return nil, err
	}
	if err := refuseConstraintOnRenamedIndexName(renamed, liveCons, desiredCons); err != nil {
		return nil, err
	}
	renameTargets := renameIndexTargets(renamed)

	// A unique index or unique constraint the plan drops is held back until
	// the plan's new unique indexes and constraints are in place, and so is
	// a constraint the desired table declares again under another name. A
	// uniqueness or rule that moves to another name or another kind of
	// object keeps holding while its replacement builds, and a replacement
	// that fails leaves the old one standing. A plan that drops or retypes a
	// column keeps the drops first, since a dropped column takes the indexes
	// and constraints that read it with it, and so does a drop whose name
	// the desired table reuses.
	held := heldDrops{
		uniqueness: addsUniqueness(desired, liveIdx, liveCons),
		liveIdx:    liveIdx, liveCons: liveCons, desiredIdx: desiredIdx, desiredCons: desiredCons,
		renamedRules: renamedConstraintDefs(desired, liveCons),
	}
	// A held foreign key waits for the foreign keys the plan adds, which
	// come last; every other held drop goes before them, so a foreign key
	// the plan adds cannot bind to a key the plan is about to drop.
	var heldChanges, heldForeignKeys []Change
	waits := func(ch Change) bool { return !dropsOrRetypes && held.holds(ch) }
	appendDrop := func(ch Change) {
		switch {
		case !waits(ch):
			changes = append(changes, ch)
		case held.dropsForeignKey(ch):
			heldForeignKeys = append(heldForeignKeys, ch)
		default:
			heldChanges = append(heldChanges, ch)
		}
	}

	// Indexes to drop: gone from desired, or changed (dropped here,
	// recreated in the create bucket below). An index a new key constraint
	// adopts stays: the constraint takes it over below. A replaced index is
	// dropped in the create bucket, once its replacement is built, and an
	// earlier run's build of a replacement stays to be renamed, or, for a
	// key, to be swapped in. A standalone index a unique constraint replaces
	// is dropped once the constraint's index is built.
	for _, ix := range live.Indexes {
		if _, ok := adopted[ix.Name]; ok {
			continue
		}
		if _, ok := replacedIdx[ix.Name]; ok {
			continue
		}
		if _, ok := claimed[ix.Name]; ok {
			continue
		}
		if _, ok := renamed[ix.Name]; ok {
			continue
		}
		if indexNeedsDrop(partitioned, ix, desiredIdx) {
			_, changed := desiredIdx[ix.Name]
			if err := refuseReferencedIndexDrop(ix, changed); err != nil {
				return nil, err
			}
			appendDrop(Change{
				SQL:         "DROP INDEX " + pgx.Identifier{schema, ix.Name}.Sanitize(),
				Kind:        ChangeDropIndex,
				Destructive: true,
				Drops:       &DroppedObject{Kind: DroppedIndex, Name: ix.Name, Definition: ix.Def},
			})
		}
	}

	// Constraints to drop: gone from desired, or changed (re-added below).
	// One left NOT VALID with the desired definition is validated below
	// instead, and one replaced in place is dropped by the statement that
	// adds its replacement. A unique constraint the desired table declares as
	// a standalone index is dropped once that index is built.
	droppedForeignKeys := make(map[string]bool)
	for _, con := range live.Constraints {
		if _, ok := replaced[con.Name]; ok {
			continue
		}
		if r, ok := replacedIdx[con.Name]; ok && r.constraint.Name != "" {
			continue
		}
		want, ok := desiredCons[con.Name]
		if !ok || (want.Def != con.Def && !validatesInPlace(con, want)) {
			if err := refuseReferencedKeyDrop(schema, live.Table, con, droppedForeignKeys); err != nil {
				return nil, err
			}
			drop := Change{
				SQL:         "ALTER TABLE " + table + " DROP CONSTRAINT " + pgx.Identifier{con.Name}.Sanitize(),
				Kind:        dropConstraintKind(con),
				Destructive: true,
				Drops:       &DroppedObject{Kind: DroppedConstraint, Name: con.Name, Definition: con.Def},
			}
			// A foreign key whose drop waits for the plan's adds is still in
			// place when a key it references is dropped, so only one dropped
			// here frees that key.
			if con.ForeignKey && !waits(drop) {
				droppedForeignKeys[con.Name] = true
			}
			appendDrop(drop)
		}
	}

	// Columns to drop. A rename is indistinguishable from drop+add at the
	// catalog level, so it surfaces as exactly that — and the drop is
	// flagged destructive for the caller to gate. Generated columns go
	// first: PostgreSQL refuses to drop a column a generated column still
	// reads, and a generated column never reads another.
	for _, generated := range []bool{true, false} {
		for _, col := range live.Columns {
			if _, ok := desiredCols[col.Name]; ok || col.IsGenerated() != generated {
				continue
			}
			changes = append(changes, Change{
				SQL:         "ALTER TABLE " + table + " DROP COLUMN " + pgx.Identifier{col.Name}.Sanitize(),
				Kind:        ChangeDropColumn,
				Destructive: true,
				Drops: &DroppedObject{
					Kind: DroppedColumn, Name: col.Name, Definition: col.DroppedDefinition(),
					Dependents: DependentLines(col.Dependents),
				},
			})
		}
	}

	// NOT NULLs to drop, after the columns dropped: a column that is part of
	// a key the plan drops has lost its NOT NULL's owner by now. A primary
	// key replaced in place still owns its columns' NOT NULL until the swap
	// drops the old key, so when the plan replaces one, the NOT NULL drops
	// wait for the constraints below. The new key is then built and swapped
	// in while the old one keeps rejecting duplicates, and a column the old
	// key covered loses its NOT NULL only once nothing requires it.
	notNullDrops := dropNotNullChanges(table, live, desiredCols)
	notNullAfterKeySwap := replacesPrimaryKey(replaced, liveCons)
	if !notNullAfterKeySwap {
		changes = append(changes, notNullDrops...)
	}

	// Columns to add. A sequence-backed default (serial) cannot be added:
	// the desired-side sequence existed only inside the rolled-back
	// scratch transaction, so the emitted default would reference a
	// relation the plan never creates.
	for _, col := range desired.Columns {
		if _, ok := liveCols[col.Name]; !ok {
			if col.SequenceDefault {
				return nil, unsupported(CauseSerialDefault, "%w: column %q has a sequence-backed default (serial); the plan cannot create its sequence", ErrUnsupportedChange, col.Name)
			}
			// columnDef carries no COLLATE clause, so emitting the add
			// would silently drop the collation — refuse instead.
			if col.Collation != "" {
				return nil, unsupported(CauseAddedColumnCollation, "%w: column %q has an explicit collation (%s); the plan cannot carry it", ErrUnsupportedChange, col.Name, col.Collation)
			}
			if err := identityExpressible(col); err != nil {
				return nil, unsupported(CauseIdentity, "%w: %w", ErrUnsupportedChange, err)
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
	// Foreign keys are held back until the table's other constraints and
	// indexes are in place, because a key can reference the primary key,
	// unique constraint, or unique index the same diff adds to its own
	// table.
	var foreignKeys []Change
	for _, con := range desired.Constraints {
		if _, ok := adopted[con.Name]; ok {
			changes = append(changes, Change{
				SQL:    adoptIndexSQL(table, con),
				Kind:   ChangeAddConstraint,
				Adopts: &IndexAdoption{Index: con.Name, Def: con.IndexDef},
			})
			continue
		}
		had, ok := liveCons[con.Name]
		if r, inPlace := replaced[con.Name]; inPlace {
			replacement, err := replaceInPlaceChanges(table, schema, had, con, r)
			if err != nil {
				return nil, err
			}
			changes = append(changes, replacement...)
			continue
		}
		if ok && validatesInPlace(had, con) {
			validate := Change{
				SQL:       "ALTER TABLE " + table + " VALIDATE CONSTRAINT " + pgx.Identifier{con.Name}.Sanitize(),
				Kind:      validateConstraintKind(con),
				Validates: &ConstraintValidation{Constraint: con.Name, Def: had.Def},
			}
			if con.ForeignKey {
				foreignKeys = append(foreignKeys, validate)
			} else {
				changes = append(changes, validate)
			}
			continue
		}
		if !ok || had.Def != con.Def {
			sql := "ALTER TABLE " + table + " ADD CONSTRAINT " + pgx.Identifier{con.Name}.Sanitize() + " " + con.Def
			if con.ForeignKey {
				qualified, err := statement.QualifyForeignKeyReferences(sql, schema)
				if err != nil {
					return nil, fmt.Errorf("qualify foreign key %s: %w", con.Name, err)
				}
				sql = qualified
				foreignKeys = append(foreignKeys, Change{SQL: sql, Kind: addConstraintKind(con), AddsNotValid: addedNotValid(con), References: con.References})
				continue
			}
			changes = append(changes, Change{SQL: sql, Kind: addConstraintKind(con), AddsNotValid: addedNotValid(con)})
		}
	}

	if notNullAfterKeySwap {
		changes = append(changes, notNullDrops...)
	}

	// Indexes to create: new, replaced after a definition change, recreated
	// after a definition change on a table whose indexes cannot be
	// replaced, or rebuilt over an unfinished build. The desired definition
	// is server-decompiled and unqualified; only the schema qualification
	// is injected.
	for _, ix := range desired.Indexes {
		if r, ok := replacedIdx[ix.Name]; ok {
			if r.constraint.Name != "" {
				if err := refuseReferencedKeyDrop(schema, live.Table, r.constraint, droppedForeignKeys); err != nil {
					return nil, err
				}
			}
			replacement, err := replaceIndexChanges(schema, table, r)
			if err != nil {
				return nil, err
			}
			changes = append(changes, replacement...)
			continue
		}
		if from, ok := renameTargets[ix.Name]; ok {
			changes = append(changes, renameIndexChange(schema, liveIdx[from], ix.Name))
			continue
		}
		if indexNeedsCreate(liveIdx, ix) {
			qualified, err := statement.Qualify(ix.Def, schema)
			if err != nil {
				return nil, fmt.Errorf("qualify index %s: %w", ix.Name, err)
			}
			changes = append(changes, Change{SQL: qualified, Kind: ChangeCreateIndex})
		}
	}

	changes = append(changes, heldChanges...)

	// Foreign keys last, once everything they can reference on this table
	// exists.
	changes = append(changes, foreignKeys...)
	changes = append(changes, heldForeignKeys...)

	return changes, nil
}

// alterColumnChanges derives the in-place column alterations between two
// versions of the same column. Identity and generation cannot be altered in
// place, so a delta there is refused as unsupported, and so is a delta in
// an identity's sequence declaration. A stored generated
// column whose expression alone differs is planned as SET EXPRESSION, the
// one statement that states the change: it keeps the column's indexes,
// where a drop and re-add would silently take them with it. The planner
// refuses that statement, so the drift is reviewable and never executes.
func alterColumnChanges(table string, live, desired Column) ([]Change, error) {
	if live.Identity != desired.Identity {
		return nil, unsupported(CauseIdentity, "%w: column %q identity change", ErrUnsupportedChange, live.Name)
	}
	if live.Generated != desired.Generated {
		return nil, unsupported(CauseGenerated, "%w: column %q generated change", ErrUnsupportedChange, live.Name)
	}
	if live.Virtual != desired.Virtual {
		return nil, unsupported(CauseGenerated, "%w: column %q virtual generated change", ErrUnsupportedChange, live.Name)
	}
	// An identity's sequence declaration is part of what the file states,
	// and the diff does not converge it, so a difference fails closed. A
	// serial's is not compared: the serial shorthand cannot state one, so
	// the desired side always carries the defaults and export refuses a
	// live serial declared otherwise.
	// A type change on the column retypes its identity sequence too, so
	// live is compared as the type change would leave it.
	if desired.Identity != IdentityNone {
		if after := identitySequenceAfterRetype(live, desired.Type); !sameSequence(after, desired.Sequence) {
			return nil, unsupported(CauseIdentitySequence, "%w: column %q identity sequence differs between live (%s) and desired (%s)",
				ErrUnsupportedChange, live.Name, formatSequence(after), formatSequence(desired.Sequence))
		}
	}
	// A collation change rewrites the column and rebuilds its indexes —
	// the engine does not derive it, so a delta fails closed instead of
	// diffing to silence.
	if live.Collation != desired.Collation {
		return nil, unsupported(CauseCollationChange, "%w: column %q collation change (%q vs %q)", ErrUnsupportedChange, live.Name, live.Collation, desired.Collation)
	}
	if desired.IsGenerated() && live.Type != desired.Type {
		return nil, unsupported(CauseGenerated, "%w: column %q generated column type change", ErrUnsupportedChange, live.Name)
	}
	col := pgx.Identifier{live.Name}.Sanitize()
	var changes []Change
	if desired.IsGenerated() && live.Default != desired.Default {
		changes = append(changes, Change{
			SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " SET EXPRESSION AS (" + desired.Default + ")",
			Kind: ChangeSetGeneratedExpression,
		})
	}
	if live.Type != desired.Type {
		changes = append(changes, Change{
			SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " TYPE " + desired.Type,
			Kind: ChangeAlterType,
		})
	}
	if !desired.IsGenerated() && desired.Identity == IdentityNone && live.Default != desired.Default {
		// A sequence-backed desired default (serial adoption) is refused:
		// the sequence existed only inside the rolled-back scratch
		// transaction, so the emitted SET DEFAULT would reference a
		// relation the plan never creates — or worse, silently bind to an
		// unrelated live sequence of the same name.
		if desired.SequenceDefault {
			return nil, unsupported(CauseSerialDefault, "%w: column %q would adopt a sequence-backed default (serial); the plan cannot create its sequence", ErrUnsupportedChange, live.Name)
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
	// A NOT NULL the desired column lacks is dropped in the drop phase of
	// Diff, not here: see dropNotNullChanges.
	if !live.NotNull && desired.NotNull {
		changes = append(changes, Change{
			SQL:  "ALTER TABLE " + table + " ALTER COLUMN " + col + " SET NOT NULL",
			Kind: ChangeSetNotNull,
		})
	}
	return changes, nil
}

// dropNotNullChanges derives the DROP NOT NULL of every column the desired
// table keeps but declares nullable. They run in the drop phase, before any
// statement that can rebuild the table: a copy-and-swap rewrite compacts the
// attribute numbers the drop's definition is made of.
func dropNotNullChanges(table string, live Model, desiredCols map[string]Column) []Change {
	var changes []Change
	for _, col := range live.Columns {
		want, ok := desiredCols[col.Name]
		if !ok || !col.NotNull || want.NotNull {
			continue
		}
		changes = append(changes, Change{
			SQL:   "ALTER TABLE " + table + " ALTER COLUMN " + pgx.Identifier{col.Name}.Sanitize() + " DROP NOT NULL",
			Kind:  ChangeDropNotNull,
			Drops: &DroppedObject{Kind: DroppedNotNull, Name: col.Name, Definition: col.DroppedNotNullDefinition()},
		})
	}
	return changes
}

// columnDef renders a canonical column definition for ADD COLUMN. All parts
// are server-canonical: the type from format_type, expressions from
// pg_get_expr.
func columnDef(c Column) string {
	def := pgx.Identifier{c.Name}.Sanitize() + " " + c.Type
	switch {
	case c.Generated:
		def += " GENERATED ALWAYS AS (" + c.Default + ") STORED"
	case c.Virtual:
		def += " GENERATED ALWAYS AS (" + c.Default + ") VIRTUAL"
	case c.Identity == IdentityAlways:
		def += " GENERATED ALWAYS AS IDENTITY" + identityOptions(c.Sequence)
	case c.Identity == IdentityByDefault:
		def += " GENERATED BY DEFAULT AS IDENTITY" + identityOptions(c.Sequence)
	case c.Default != "":
		def += " DEFAULT " + c.Default
	}
	if c.NotNull {
		def += " NOT NULL"
	}
	return def
}

// identityOptions renders an identity's sequence options clause, empty
// when the sequence is declared at the defaults. The sequence's type is
// not stated: it is always the column's, which identityExpressible checks.
func identityOptions(seq *SequenceOptions) string {
	if seq == nil {
		return ""
	}
	return " (" + sequenceClause(*seq) + ")"
}

// sequenceClause spells every declared option, so the declaration does not
// depend on which defaults the server would fill in.
func sequenceClause(seq SequenceOptions) string {
	cycle := "NO CYCLE"
	if seq.Cycle {
		cycle = "CYCLE"
	}
	return fmt.Sprintf("START WITH %d INCREMENT BY %d MINVALUE %d MAXVALUE %d CACHE %d %s",
		seq.Start, seq.Increment, seq.Min, seq.Max, seq.Cache, cycle)
}

// identitySequenceAfterRetype returns the declaration live's identity
// sequence carries once ALTER COLUMN ... TYPE gives the column type to. The
// server retypes the identity sequence with its column, and a bound equal
// to the sequence's old type's own limit was never chosen by anyone, so it
// follows the new type; a declared bound stays as written. That is the
// same rule the copy-and-swap cutover applies to the sequences it hands
// over. A column keeping its type, or a type that is not an integer type,
// leaves the declaration as it is.
func identitySequenceAfterRetype(live Column, to string) *SequenceOptions {
	if live.Type == to {
		return live.Sequence
	}
	opts, ok := defaultSequenceOptions(live.Type)
	if live.Sequence != nil {
		opts, ok = *live.Sequence, true
	}
	fromMin, fromMax, fromOK := integerTypeRange(opts.Type)
	toMin, toMax, toOK := integerTypeRange(to)
	if !ok || !fromOK || !toOK {
		return live.Sequence
	}
	opts.Type = to
	if opts.Min == fromMin {
		opts.Min = toMin
	}
	if opts.Max == fromMax {
		opts.Max = toMax
	}
	return declaredSequence(to, opts)
}

// integerTypeRange returns the smallest and largest value of an integer
// type a sequence can have, and false for any other type.
func integerTypeRange(typ string) (minValue, maxValue int64, ok bool) {
	switch typ {
	case "smallint":
		return math.MinInt16, math.MaxInt16, true
	case "integer":
		return math.MinInt32, math.MaxInt32, true
	case "bigint":
		return math.MinInt64, math.MaxInt64, true
	default:
		return 0, 0, false
	}
}

// sameSequence reports whether two recorded sequence declarations match;
// nil is the default declaration on both sides.
func sameSequence(a, b *SequenceOptions) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// formatSequence describes a recorded sequence declaration for an error
// message.
func formatSequence(seq *SequenceOptions) string {
	if seq == nil {
		return "the defaults"
	}
	return seq.Type + " " + sequenceClause(*seq)
}

// formatOptions lists storage parameters for an error message, "none" when
// there are none.
func formatOptions(opts []string) string {
	if len(opts) == 0 {
		return "none"
	}
	return strings.Join(opts, ", ")
}

// columnsByName indexes columns for lookup during the diff.
func columnsByName(cols []Column) map[string]Column {
	m := make(map[string]Column, len(cols))
	for _, c := range cols {
		m[c.Name] = c
	}
	return m
}

// adoptableIndexes names the live indexes a new primary key or unique
// constraint can take over with ADD CONSTRAINT ... USING INDEX, keyed by
// index name, which is also the constraint's. Dropping such an index and
// letting the constraint build its own would leave the table without the
// uniqueness guarantee and without the index for as long as the rebuild
// runs. A live index qualifies when it has the constraint's name and is
// exactly the index the constraint would build, so adopting it leaves the
// table as building the constraint fresh would. An unfinished build keeps
// the drop and rebuild, and so does a partitioned parent, where the server
// refuses USING INDEX.
func adoptableIndexes(partitioned bool, liveCons map[string]Constraint, liveIdx map[string]Index, desired []Constraint) map[string]struct{} {
	adopted := make(map[string]struct{})
	if partitioned {
		return adopted
	}
	for _, con := range desired {
		if con.IndexDef == "" {
			continue
		}
		if _, ok := liveCons[con.Name]; ok {
			continue
		}
		ix, ok := liveIdx[con.Name]
		if !ok || ix.Invalid || ix.Def != con.IndexDef {
			continue
		}
		adopted[con.Name] = struct{}{}
	}
	return adopted
}

// adoptIndexSQL is the statement that makes a primary key or unique
// constraint take over the live index of its own name, with the
// constraint's declared deferrability: the server marks the index
// deferred as it attaches it.
func adoptIndexSQL(table string, con Constraint) string {
	return "ALTER TABLE " + table + " " + addUsingIndexClause(con, con.Name)
}

// addUsingIndexClause is the ADD CONSTRAINT clause that makes con take over
// index, with the constraint's declared deferrability. When index has
// another name, the server renames it to the constraint's.
func addUsingIndexClause(con Constraint, index string) string {
	key := "UNIQUE"
	if con.Primary {
		key = "PRIMARY KEY"
	}
	clause := "ADD CONSTRAINT " + pgx.Identifier{con.Name}.Sanitize() + " " + key + " USING INDEX " + pgx.Identifier{index}.Sanitize()
	if con.Deferrable {
		clause += " DEFERRABLE"
	}
	if con.InitiallyDeferred {
		clause += " INITIALLY DEFERRED"
	}
	return clause
}

// dropConstraintKind classifies a constraint drop, keeping foreign keys in
// their own kind so desired-state execution can refuse them.
func dropConstraintKind(con Constraint) ChangeKind {
	if con.ForeignKey {
		return ChangeDropForeignKey
	}
	return ChangeDropConstraint
}

// addConstraintKind classifies a constraint add the same way.
func addConstraintKind(con Constraint) ChangeKind {
	if con.ForeignKey {
		return ChangeAddForeignKey
	}
	return ChangeAddConstraint
}

// validateConstraintKind classifies a constraint validation the same way.
func validateConstraintKind(con Constraint) ChangeKind {
	if con.ForeignKey {
		return ChangeValidateForeignKey
	}
	return ChangeValidateConstraint
}

// notValidSuffix is what pg_get_constraintdef appends to the definition of a
// constraint the server has not validated, and the only difference from the
// definition of the same constraint validated.
const notValidSuffix = " NOT VALID"

// addedNotValid is the record an added CHECK or FOREIGN KEY carries of the
// definition it has once added NOT VALID: its desired definition with the
// suffix the server prints for an unvalidated constraint. Nil for every other
// constraint, which the server cannot add NOT VALID.
func addedNotValid(con Constraint) *ConstraintValidation {
	if !con.ForeignKey && !strings.HasPrefix(con.Def, "CHECK ") {
		return nil
	}
	return &ConstraintValidation{Constraint: con.Name, Def: con.Def + notValidSuffix}
}

// validatesInPlace reports whether live is the desired constraint left NOT
// VALID, so validating it converges the two: the drop and re-add the
// definitions' difference would otherwise plan is destructive, and the
// re-add scans the table again.
func validatesInPlace(live, desired Constraint) bool {
	return live.NotValid && !desired.NotValid && live.Def == desired.Def+notValidSuffix
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

// indexNeedsCreate reports whether the desired index must be built: the
// live table lacks it, the live definition differs, or the live entry with
// the desired name and definition is invalid and so does not deliver the
// index. On a plain table that entry is an unfinished concurrent build; on a
// partitioned parent it is an unfinished partition-by-partition build, whose
// definition matching the desired one is what makes the same CREATE INDEX a
// resume rather than a collision.
func indexNeedsCreate(liveIdx map[string]Index, want Index) bool {
	had, ok := liveIdx[want.Name]
	if !ok {
		return true
	}
	if had.Def != want.Def {
		return true
	}
	return had.Invalid
}

// indexNeedsDrop reports whether a live index must be dropped: desired no
// longer names it, or names it with a different definition. An unfinished
// concurrent build is never dropped here, whatever desired says about its
// name: DROP INDEX blocks the table and cannot tell abandoned debris from a
// build still in progress. Its removal belongs to the concurrent build
// path, which proves the occupant before touching it. partitioned says
// whether the live table is a partitioned parent, whose invalid entries
// are not unfinished builds.
func indexNeedsDrop(partitioned bool, ix Index, desired map[string]Index) bool {
	if isUnfinishedBuild(partitioned, ix) {
		return false
	}
	want, ok := desired[ix.Name]
	if !ok {
		return true
	}
	return want.Def != ix.Def
}

// isUnfinishedBuild reports whether a live index entry is a concurrent
// build that has not finished — abandoned, or still running. That is what
// an invalid entry means on a plain table; a partitioned parent's invalid
// entry means something else, and is never one.
func isUnfinishedBuild(partitioned bool, ix Index) bool {
	if partitioned {
		return false
	}
	return ix.Invalid
}
