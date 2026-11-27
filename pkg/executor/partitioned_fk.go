// This file is the partitioned foreign key build: the online way to add a
// FOREIGN KEY to a partitioned table on PostgreSQL 14 to 17. Those versions
// refuse ADD FOREIGN KEY ... NOT VALID on a partitioned table, so the plain
// statement validates every partition while it holds SHARE ROW EXCLUSIVE on
// the tree, which blocks every writer for the whole scan. The same plain
// statement adopts a partition's validated foreign key of the same
// definition instead of creating and validating one, so the build first
// gives every leaf that foreign key online (ensureLeafForeignKey), then runs
// the parent's ADD FOREIGN KEY, which only changes the catalog
// (docs/partitioned-foreign-key-design.md).

package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// Typed refusals of the partitioned foreign key build as a whole. Each is
// returned before the build creates anything, except
// ErrPartitionedForeignKeyUnsettled and ErrPartitionedForeignKeyLeafChanged,
// which leave every partition's finished foreign key in place for a rerun to
// adopt.
var (
	// ErrNotAddForeignKey is returned when the statement is not one ALTER
	// TABLE with exactly one ADD FOREIGN KEY subcommand.
	ErrNotAddForeignKey = errors.New("statement is not a single ALTER TABLE ... ADD FOREIGN KEY")
	// ErrPartitionedForeignKeyUnsupportedShape is returned when the ADD
	// FOREIGN KEY names no constraint or carries a clause the build cannot
	// carry to every partition: an unqualified referenced table, no
	// referenced column list or one of another length, an ON DELETE column
	// list, NOT VALID, ONLY, or a repeated column. Each partition's foreign key takes
	// a name derived from the constraint's, and a rerun finds it by that
	// name, so an unnamed constraint could not be resumed.
	ErrPartitionedForeignKeyUnsupportedShape = errors.New("the partitioned foreign key build takes a named FOREIGN KEY over distinct columns, referencing a schema-qualified table's listed columns, with no ON DELETE column list, NOT VALID, or ONLY")
	// ErrPartitionedForeignKeyParentConflict is returned when the
	// partitioned table already has a constraint under the requested name
	// that is not the requested validated foreign key, or already has the
	// requested foreign key under another name.
	ErrPartitionedForeignKeyParentConflict = errors.New("the partitioned table already has a constraint that stands in the way of the requested foreign key")
	// ErrPartitionedForeignKeySubPartitioned is returned when a partition of
	// the target is itself partitioned. The build gives the foreign key to
	// leaf partitions directly under the target only.
	ErrPartitionedForeignKeySubPartitioned = errors.New("a partition of the target is itself partitioned, and the partitioned foreign key build takes only leaf partitions directly under the target")
	// ErrPartitionedForeignKeyUnsettled is returned when partitions kept
	// appearing without the foreign key for as long as the build's bounded
	// number of passes. Every partition's finished foreign key stays, and a
	// rerun resumes.
	ErrPartitionedForeignKeyUnsettled = errors.New("partitions kept appearing without the foreign key until the build ran out of passes")
	// ErrPartitionedForeignKeyLeafChanged is returned when the parent's step
	// finds, under the tree's lock, that a partition no longer carries the
	// foreign key its leaf step left: something dropped, replaced, or added
	// a foreign key of the definition on that partition while the build
	// ran. The step rolls back before the parent's statement, every
	// partition's foreign key stays, and a rerun decides each partition
	// again from what it finds.
	ErrPartitionedForeignKeyLeafChanged = errors.New("a partition's foreign key changed after its leaf step, before the parent's foreign key was added")
	// ErrPartitionedForeignKeyNotValidSupported is returned on PostgreSQL 18
	// and later, which accepts ADD FOREIGN KEY ... NOT VALID on a
	// partitioned table and validates it with VALIDATE CONSTRAINT under
	// SHARE UPDATE EXCLUSIVE. The ordinary online foreign key route, NOT
	// VALID then VALIDATE, is the way to add it there.
	ErrPartitionedForeignKeyNotValidSupported = errors.New("the server accepts NOT VALID on a partitioned table's foreign key, so the ordinary NOT VALID then VALIDATE route adds it online")
)

// partitionedFKNotValidMajor is the first PostgreSQL major that accepts ADD
// FOREIGN KEY ... NOT VALID on a partitioned table.
const partitionedFKNotValidMajor = 18

// PartitionForeignKeyAction says what the build did to give one partition the
// foreign key the parent's adopts.
type PartitionForeignKeyAction string

const (
	// PartitionForeignKeyFound means the partition already had the validated
	// foreign key, attached to no parent's: an earlier run's, or one an
	// operator made.
	PartitionForeignKeyFound PartitionForeignKeyAction = "found"
	// PartitionForeignKeyValidated means the partition had the foreign key
	// NOT VALID, and the build validated it.
	PartitionForeignKeyValidated PartitionForeignKeyAction = "validated"
	// PartitionForeignKeyAdded means the build added the partition's
	// foreign key NOT VALID, then validated it.
	PartitionForeignKeyAdded PartitionForeignKeyAction = "added"
)

// PartitionForeignKeyResult is one leaf partition's foreign key as the build
// left it for the parent's to adopt.
type PartitionForeignKeyResult struct {
	// Schema and Table identify the partition.
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Constraint and ConstraintOID identify the partition's foreign key. The
	// parent's foreign key adopts it as it is, so its OID does not change.
	Constraint    string `json:"constraint"`
	ConstraintOID uint32 `json:"constraint_oid"`
	// Action says how the partition got its foreign key.
	Action PartitionForeignKeyAction `json:"action"`
}

// PartitionedForeignKeyReport says what a partitioned foreign key build did.
// It is returned only after the executor read back, in the transaction that
// added it, that the parent's foreign key adopted every partition's without
// creating or validating one.
type PartitionedForeignKeyReport struct {
	// Schema and Table identify the partitioned table.
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Constraint and ConstraintOID identify the parent's foreign key.
	Constraint    string `json:"constraint"`
	ConstraintOID uint32 `json:"constraint_oid"`
	// ParentAdded is true when this run added the parent's foreign key, and
	// false when an earlier run had already added it.
	ParentAdded bool `json:"parent_added"`
	// Partitions is the number of partitions in the tree.
	Partitions int `json:"partitions"`
	// Leaves lists every partition the run gave the foreign key, in the
	// order it first reached them. It is empty when the parent's foreign key
	// already existed.
	Leaves []PartitionForeignKeyResult `json:"leaves,omitempty"`
	// Passes is how many times the build walked the tree.
	Passes int `json:"passes"`
	// Duration is the wall-clock time of the whole build. It encodes as
	// integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
	// ServerVersion is the server_version of the server that ran the build.
	ServerVersion string `json:"server_version"`
}

// maxPartitionedForeignKeyPasses bounds how many times the build walks the
// partition tree. A pass gives every listed leaf its foreign key and then
// adds the parent's; it defers the parent to another pass only when a
// partition appeared that it did not cover.
const maxPartitionedForeignKeyPasses = 5

// AddPartitionedForeignKey adds the FOREIGN KEY an ALTER TABLE statement
// describes to a partitioned table, online, on PostgreSQL 14 to 17, and
// returns once the parent's foreign key exists and has adopted a validated
// foreign key on every partition.
//
// The statement is one schema-qualified ALTER TABLE parent ADD CONSTRAINT
// name FOREIGN KEY (columns) REFERENCES schema.table (columns), with any
// MATCH SIMPLE or FULL, ON UPDATE and ON DELETE action, and deferrability,
// on the preflighted partitioned table. The build holds the parent's
// advisory table lock (LK-1) for its whole run, so a second run on the same
// parent is refused with a *dbconn.TableLockHeldError; cfg is the connection
// target that lock's dedicated session dials, the same Config the pool was
// made from. Under the lock it:
//
//  1. refuses a server that accepts NOT VALID on a partitioned table's
//     foreign key (ErrPartitionedForeignKeyNotValidSupported), a referenced
//     table that is not a plain table outside the tree
//     (ErrPartitionedForeignKeyReferencedTable), and a parent that already
//     has another constraint under the name or the same foreign key under
//     another name (ErrPartitionedForeignKeyParentConflict); a parent that
//     already has exactly the requested foreign key is reported done;
//  2. refuses a tree that holds a foreign table (ErrForeignTablePartition),
//     a sub-partitioned partition (ErrPartitionedForeignKeySubPartitioned),
//     or an unfinished detach (ErrPartitionDetachPending);
//  3. gives each leaf the foreign key: ADD FOREIGN KEY ... NOT VALID in a
//     brief transaction, then VALIDATE CONSTRAINT bounded by b, resumed from
//     whatever an earlier run left;
//  4. in one brief transaction, locks the tree and the referenced table,
//     proves every partition has exactly the validated foreign key step 3
//     recorded, runs the parent's ADD FOREIGN KEY, and reads back that it
//     adopted each of them; a partition that appeared in the meantime sends
//     the build round again, up to a fixed number of passes
//     (ErrPartitionedForeignKeyUnsettled when they keep appearing).
//
// A failure at any point leaves every finished partition's foreign key, and
// rerunning the same statement resumes from them. Every statement but the
// VALIDATEs runs under the pool's own bounded lock and statement timeouts;
// the pool must come from pkg/dbconn.
func AddPartitionedForeignKey(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ValidateBudget) (PartitionedForeignKeyReport, error) {
	return addPartitionedFKAt(ctx, cfg, pool, pt, sql, b, nil, 0)
}

// AddPartitionedForeignKeyAsStep is AddPartitionedForeignKey reported to the
// run step at: the whole build is step at.Step of a run that owns the
// tracker, so each partition's statements and the parent's are each a
// statement of that one step, and the run's step total is untouched. The
// call never calls Start or Finish.
func AddPartitionedForeignKeyAsStep(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ValidateBudget, at RunStep) (PartitionedForeignKeyReport, error) {
	if err := at.validate(); err != nil {
		return PartitionedForeignKeyReport{}, err
	}
	return addPartitionedFKAt(ctx, cfg, pool, pt, sql, b, at.Tracker, at.Step)
}

func addPartitionedFKAt(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, b ValidateBudget, tracker *progress.Tracker, runStep int) (PartitionedForeignKeyReport, error) {
	var rep PartitionedForeignKeyReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	req, err := admitPartitionedFK(sql, pt)
	if err != nil {
		return rep, err
	}
	start := time.Now()
	if tracker != nil {
		start = tracker.Now()
	}
	ran := false
	err = runUnderParentLock(ctx, cfg, pool, req.schema, req.table, "the partitioned foreign key build", func(lockedCtx context.Context) error {
		ran = true
		var runErr error
		rep, runErr = runPartitionedFK(lockedCtx, pool, req, b, tracker, runStep)
		return runErr
	})
	// A build refused before it held the lock ran nothing and reports no
	// duration.
	if !ran {
		return rep, err
	}
	rep.Duration = elapsedSince(tracker, start)
	return rep, err
}

// partitionedFKRequest is the admitted statement: the parent it targets and
// the foreign key it asks for.
type partitionedFKRequest struct {
	schema string
	table  string
	spec   partitionedFKSpec
}

// admitPartitionedFK is the executor's own admission check: the statement's
// shape, on the schema-qualified table the preflight verified, which must be
// partitioned. Each refusal is typed and nothing is executed on one.
func admitPartitionedFK(sql string, pt preflight.PreflightedTable) (partitionedFKRequest, error) {
	st, spec, err := admitPartitionedFKShape(sql)
	if err != nil {
		return partitionedFKRequest{}, err
	}
	// INV: ST-7 — the executor runs exactly the statement that was gated, and
	// only against the table the preflight proof verified.
	if st.Schema() != pt.Schema() || st.Table() != pt.Table() {
		return partitionedFKRequest{}, fmt.Errorf("%w: ST-7: statement targets %q but preflight verified %q",
			ErrInvariantViolation, qualifiedName(st.Schema(), st.Table()), qualifiedName(pt.Schema(), pt.Table()))
	}
	if !pt.Partitioned() {
		return partitionedFKRequest{}, fmt.Errorf("%w: %s", ErrNotPartitionedTable, qualifiedName(pt.Schema(), pt.Table()))
	}
	return partitionedFKRequest{schema: st.Schema(), table: st.Table(), spec: spec}, nil
}

// AdmitPartitionedForeignKeyShape is the statement-shape half of the
// partitioned foreign key build's admission: exactly one schema-qualified
// ALTER TABLE with one named ADD CONSTRAINT ... FOREIGN KEY over distinct
// columns, referencing a schema-qualified table's listed columns, with no ON
// DELETE column list, NOT VALID, or ONLY. It needs no
// connection, so a dry run learns the shape refusals the build would return
// before it runs anything: ErrNotAddForeignKey and
// ErrPartitionedForeignKeyUnsupportedShape, typed. A statement the parser
// rejects, such as MATCH PARTIAL or more than one statement, is returned as
// the parser's error, untyped.
func AdmitPartitionedForeignKeyShape(sql string) error {
	_, _, err := admitPartitionedFKShape(sql)
	return err
}

// admitPartitionedFKShape returns the parsed statement and the foreign key it
// asks for. The build never runs the statement's text: every statement is
// rendered from the spec, so every clause the spec cannot hold is refused
// here.
func admitPartitionedFKShape(sql string) (statement.Statement, partitionedFKSpec, error) {
	st, err := statement.ParseOne(sql)
	if err != nil {
		return statement.Statement{}, partitionedFKSpec{}, err
	}
	if st.Kind() != statement.KindAlterTable {
		return statement.Statement{}, partitionedFKSpec{}, fmt.Errorf("%w: got %s", ErrNotAddForeignKey, st.Kind())
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return statement.Statement{}, partitionedFKSpec{}, err
	}
	if len(ops) != 1 {
		return statement.Statement{}, partitionedFKSpec{}, fmt.Errorf("%w: the statement has %d subcommands", ErrNotAddForeignKey, len(ops))
	}
	op := ops[0]
	if op.Kind != statement.OpAddConstraint || op.AddedForeignKey == nil {
		return statement.Statement{}, partitionedFKSpec{}, fmt.Errorf("%w: got %s", ErrNotAddForeignKey, op.Describe())
	}
	if st.Schema() == "" {
		return statement.Statement{}, partitionedFKSpec{}, ErrUnqualifiedTable
	}
	if op.Name == "" {
		return statement.Statement{}, partitionedFKSpec{}, fmt.Errorf("%w: the foreign key has no name; name it, because each partition's foreign key takes a name derived from it and a rerun finds it by that name",
			ErrPartitionedForeignKeyUnsupportedShape)
	}
	if clauses := unsupportedFKClauses(op); len(clauses) > 0 {
		return statement.Statement{}, partitionedFKSpec{}, fmt.Errorf("%w: foreign key %s has %s",
			ErrPartitionedForeignKeyUnsupportedShape, op.Name, strings.Join(clauses, ", "))
	}
	fk := op.AddedForeignKey
	spec := partitionedFKSpec{
		name: op.Name, columns: fk.Columns,
		refSchema: fk.RefSchema, refTable: fk.RefTable, refColumns: fk.RefColumns,
		match: fk.Match, onUpdate: fk.OnUpdate, onDelete: fk.OnDelete,
		deferrable: fk.Deferrable, initiallyDeferred: fk.InitiallyDeferred,
	}
	if err := spec.validate(); err != nil {
		return statement.Statement{}, partitionedFKSpec{}, err
	}
	return st, spec, nil
}

// unsupportedFKClauses names every clause of an ADD FOREIGN KEY that the
// build cannot carry to the partitions' foreign keys.
func unsupportedFKClauses(op statement.Op) []string {
	fk := op.AddedForeignKey
	var clauses []string
	if op.Only {
		clauses = append(clauses, "ONLY, which would add the parent's foreign key without the partitions'")
	}
	if op.NotValid {
		clauses = append(clauses, "NOT VALID, which PostgreSQL refuses on a partitioned table before version 18")
	}
	if fk.RefSchema == "" {
		clauses = append(clauses, "an unqualified referenced table "+fk.RefTable)
	}
	if len(fk.RefColumns) == 0 {
		clauses = append(clauses, "no referenced column list")
	}
	if len(fk.RefColumns) > 0 && len(fk.RefColumns) != len(fk.Columns) {
		clauses = append(clauses, fmt.Sprintf("%d columns referencing %d", len(fk.Columns), len(fk.RefColumns)))
	}
	if len(fk.DeleteSetColumns) > 0 {
		clauses = append(clauses, "an ON DELETE column list")
	}
	seen := make(map[string]struct{}, len(fk.Columns))
	for _, c := range fk.Columns {
		if _, dup := seen[c]; dup {
			clauses = append(clauses, fmt.Sprintf("column %q named twice", c))
		}
		seen[c] = struct{}{}
	}
	return clauses
}
