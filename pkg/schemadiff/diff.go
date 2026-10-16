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
// surfaces it; nothing is guessed.
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
	// ChangeDropConstraint drops a table constraint other than a foreign
	// key.
	ChangeDropConstraint ChangeKind = "drop-constraint"
	// ChangeDropForeignKey drops a foreign-key constraint. Desired-state
	// execution refuses it: foreign-key changes are planned, not run.
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
	// ChangeAddForeignKey adds a foreign-key constraint. Desired-state
	// execution refuses it: foreign-key changes are planned, not run.
	ChangeAddForeignKey ChangeKind = "add-foreign-key"
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
		ChangeCreateIndex,
	}
}

// IsForeignKey reports whether the kind adds or drops a foreign key.
func (k ChangeKind) IsForeignKey() bool {
	return k == ChangeAddForeignKey || k == ChangeDropForeignKey
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
}

// IndexAdoption is an index a new primary key or unique constraint takes
// over, and the definition, as introspection reads it (pg_get_indexdef with
// the schema qualification removed), that the index must still have when
// it does.
type IndexAdoption struct {
	Index string
	Def   string
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
// RowSecurity is observational only: table-only desired files do not claim
// ownership of access control, so missing policies never produce policy drops.
func Diff(schema string, live, desired Model) ([]Change, error) {
	if live.Table != desired.Table {
		return nil, fmt.Errorf("%w: %q vs %q", ErrDifferentTables, live.Table, desired.Table)
	}
	if desired.RowSecurity.present() {
		return nil, fmt.Errorf("desired row security is not managed: %w", ErrUnsupportedChange)
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
	// Persistence is convergeable only by a full table rewrite (SET LOGGED
	// / SET UNLOGGED), which the engine does not derive — a mismatch fails
	// closed instead of diffing to silence.
	if live.Unlogged != desired.Unlogged {
		return nil, fmt.Errorf("table persistence (unlogged) differs between live and desired: %w", ErrUnsupportedChange)
	}
	// Storage parameters are not converged: a mismatch fails closed, so a
	// parameter the file declares is never reported as applied and a tuned
	// live parameter the file omits is never reported as matching.
	if !slices.Equal(live.Options, desired.Options) {
		return nil, fmt.Errorf("table storage parameters differ between live (%s) and desired (%s); declare the live parameters in the desired file: %w",
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
	// has a matching attached index, a state the one-table model does not
	// describe, so the parent's entries are compared by name and definition
	// alone. Only a plain table's invalid entry is an unfinished build.
	partitioned := live.PartitionKey != ""

	adopted := adoptableIndexes(partitioned, liveCons, liveIdx, desired.Constraints)

	// Indexes to drop: gone from desired, or changed (dropped here,
	// recreated in the create bucket below). An index a new key constraint
	// adopts stays: the constraint takes it over below.
	for _, ix := range live.Indexes {
		if _, ok := adopted[ix.Name]; ok {
			continue
		}
		if indexNeedsDrop(partitioned, ix, desiredIdx) {
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
				Kind:        dropConstraintKind(con),
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
			// columnDef carries no COLLATE clause, so emitting the add
			// would silently drop the collation — refuse instead.
			if col.Collation != "" {
				return nil, fmt.Errorf("%w: column %q has an explicit collation (%s); the plan cannot carry it", ErrUnsupportedChange, col.Name, col.Collation)
			}
			if err := identityExpressible(col); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrUnsupportedChange, err)
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
		if _, ok := adopted[con.Name]; ok {
			changes = append(changes, Change{
				SQL:    adoptIndexSQL(table, con),
				Kind:   ChangeAddConstraint,
				Adopts: &IndexAdoption{Index: con.Name, Def: con.IndexDef},
			})
			continue
		}
		had, ok := liveCons[con.Name]
		if !ok || had.Def != con.Def {
			sql := "ALTER TABLE " + table + " ADD CONSTRAINT " + pgx.Identifier{con.Name}.Sanitize() + " " + con.Def
			if con.ForeignKey {
				qualified, err := statement.QualifyForeignKeyReferences(sql, schema)
				if err != nil {
					return nil, fmt.Errorf("qualify foreign key %s: %w", con.Name, err)
				}
				sql = qualified
			}
			changes = append(changes, Change{SQL: sql, Kind: addConstraintKind(con)})
		}
	}

	// Indexes to create: new, recreated after a definition change, or
	// rebuilt over an unfinished build. The desired definition is
	// server-decompiled and unqualified; only the schema qualification is
	// injected.
	for _, ix := range desired.Indexes {
		if indexNeedsCreate(partitioned, liveIdx, ix) {
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
// place, so a delta there is refused as unsupported, and so is a delta in
// an identity's sequence declaration. A stored generated
// column whose expression alone differs is planned as SET EXPRESSION, the
// one statement that states the change: it keeps the column's indexes,
// where a drop and re-add would silently take them with it. The planner
// refuses that statement, so the drift is reviewable and never executes.
func alterColumnChanges(table string, live, desired Column) ([]Change, error) {
	if live.Identity != desired.Identity {
		return nil, fmt.Errorf("%w: column %q identity change", ErrUnsupportedChange, live.Name)
	}
	if live.Generated != desired.Generated {
		return nil, fmt.Errorf("%w: column %q generated change", ErrUnsupportedChange, live.Name)
	}
	if live.Virtual != desired.Virtual {
		return nil, fmt.Errorf("%w: column %q virtual generated change", ErrUnsupportedChange, live.Name)
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
			return nil, fmt.Errorf("%w: column %q identity sequence differs between live (%s) and desired (%s)",
				ErrUnsupportedChange, live.Name, formatSequence(after), formatSequence(desired.Sequence))
		}
	}
	// A collation change rewrites the column and rebuilds its indexes —
	// the engine does not derive it, so a delta fails closed instead of
	// diffing to silence.
	if live.Collation != desired.Collation {
		return nil, fmt.Errorf("%w: column %q collation change (%q vs %q)", ErrUnsupportedChange, live.Name, live.Collation, desired.Collation)
	}
	if desired.IsGenerated() && live.Type != desired.Type {
		return nil, fmt.Errorf("%w: column %q generated column type change", ErrUnsupportedChange, live.Name)
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
// table as building the constraint fresh would. A deferrable constraint,
// an unfinished build, and a partitioned parent, where the server refuses
// USING INDEX, keep the drop and rebuild.
func adoptableIndexes(partitioned bool, liveCons map[string]Constraint, liveIdx map[string]Index, desired []Constraint) map[string]struct{} {
	adopted := make(map[string]struct{})
	if partitioned {
		return adopted
	}
	for _, con := range desired {
		if con.IndexDef == "" || con.Deferrable {
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
// constraint take over the live index of its own name.
func adoptIndexSQL(table string, con Constraint) string {
	key := "UNIQUE"
	if con.Primary {
		key = "PRIMARY KEY"
	}
	name := pgx.Identifier{con.Name}.Sanitize()
	return "ALTER TABLE " + table + " ADD CONSTRAINT " + name + " " + key + " USING INDEX " + name
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
// the desired name and definition is an unfinished concurrent build and so
// does not deliver the index. partitioned says whether the live table is a
// partitioned parent, whose invalid entries are not unfinished builds.
func indexNeedsCreate(partitioned bool, liveIdx map[string]Index, want Index) bool {
	had, ok := liveIdx[want.Name]
	if !ok {
		return true
	}
	if had.Def != want.Def {
		return true
	}
	return isUnfinishedBuild(partitioned, had)
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
