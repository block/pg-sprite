package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// The leaf step of adding a FOREIGN KEY to a partitioned table online.
// Before version 18, PostgreSQL refuses ADD FOREIGN KEY ... NOT VALID on a
// partitioned table, so the plain statement validates every partition while
// it holds SHARE ROW EXCLUSIVE on the tree, which blocks every writer for
// the whole scan. The plain statement on the parent adopts a partition's
// validated foreign key of the same definition instead of creating and
// validating one, so the flow gives every leaf that foreign key first: ADD
// FOREIGN KEY ... NOT VALID, a brief catalog change, then VALIDATE
// CONSTRAINT, which scans under SHARE UPDATE EXCLUSIVE while writes go on.
// This file is that step for one leaf.

// partitionedFKSpec is the requested foreign key: the parent's constraint
// name and the definition every leaf's foreign key must share with it for
// the parent's to adopt it. match, onUpdate, and onDelete hold the
// one-letter codes pg_constraint records (confmatchtype, confupdtype,
// confdeltype).
type partitionedFKSpec struct {
	name              string
	columns           []string
	refSchema         string
	refTable          string
	refColumns        []string
	match             string
	onUpdate          string
	onDelete          string
	deferrable        bool
	initiallyDeferred bool
}

// fkMatchClause is the MATCH form of a confmatchtype code, and false for a
// code the step does not render. PostgreSQL does not implement MATCH
// PARTIAL.
func fkMatchClause(code string) (string, bool) {
	switch code {
	case "s":
		return "MATCH SIMPLE", true
	case "f":
		return "MATCH FULL", true
	default:
		return "", false
	}
}

// fkActionClause is the referential action of a confupdtype or confdeltype
// code, and false for a code the step does not render.
func fkActionClause(code string) (string, bool) {
	switch code {
	case "a":
		return "NO ACTION", true
	case "r":
		return "RESTRICT", true
	case "c":
		return "CASCADE", true
	case "n":
		return "SET NULL", true
	case "d":
		return "SET DEFAULT", true
	default:
		return "", false
	}
}

// validate refuses a spec the step cannot act on.
func (s partitionedFKSpec) validate() error {
	if s.name == "" {
		return fmt.Errorf("%w: the foreign key has no name", ErrInvariantViolation)
	}
	if len(s.columns) == 0 || len(s.columns) != len(s.refColumns) {
		return fmt.Errorf("%w: foreign key %s has %d columns referencing %d",
			ErrInvariantViolation, s.name, len(s.columns), len(s.refColumns))
	}
	if s.refSchema == "" || s.refTable == "" {
		return fmt.Errorf("%w: foreign key %s does not name a schema-qualified referenced table", ErrInvariantViolation, s.name)
	}
	if err := s.validateCodes(); err != nil {
		return err
	}
	if s.initiallyDeferred && !s.deferrable {
		return fmt.Errorf("%w: foreign key %s is INITIALLY DEFERRED but not DEFERRABLE", ErrInvariantViolation, s.name)
	}
	seen := make(map[string]struct{}, len(s.columns))
	for _, c := range s.columns {
		if _, dup := seen[c]; dup {
			return fmt.Errorf("%w: foreign key %s names column %q twice", ErrInvariantViolation, s.name, c)
		}
		seen[c] = struct{}{}
	}
	return nil
}

// validateCodes refuses a MATCH or action code the step cannot render.
func (s partitionedFKSpec) validateCodes() error {
	if _, ok := fkMatchClause(s.match); !ok {
		return fmt.Errorf("%w: foreign key %s has match type %q", ErrInvariantViolation, s.name, s.match)
	}
	for _, action := range []string{s.onUpdate, s.onDelete} {
		if _, ok := fkActionClause(action); !ok {
			return fmt.Errorf("%w: foreign key %s has referential action %q", ErrInvariantViolation, s.name, action)
		}
	}
	return nil
}

// clause renders the foreign key's definition with every identifier quoted
// and every rule spelled out, so the leaf's foreign key and the parent's
// carry exactly the spec and nothing else. It is called only on a spec that
// validate accepted.
func (s partitionedFKSpec) clause() string {
	match, _ := fkMatchClause(s.match)
	onUpdate, _ := fkActionClause(s.onUpdate)
	onDelete, _ := fkActionClause(s.onDelete)
	deferrable := "NOT DEFERRABLE"
	if s.deferrable {
		deferrable = "DEFERRABLE"
	}
	initially := "INITIALLY IMMEDIATE"
	if s.initiallyDeferred {
		initially = "INITIALLY DEFERRED"
	}
	return "FOREIGN KEY (" + quotedColumns(s.columns) + ") REFERENCES " +
		pgx.Identifier{s.refSchema, s.refTable}.Sanitize() + " (" + quotedColumns(s.refColumns) + ") " +
		match + " ON UPDATE " + onUpdate + " ON DELETE " + onDelete + " " + deferrable + " " + initially
}

// quotedColumns joins column names, each quoted.
func quotedColumns(columns []string) string {
	quoted := make([]string, 0, len(columns))
	for _, c := range columns {
		quoted = append(quoted, pgx.Identifier{c}.Sanitize())
	}
	return strings.Join(quoted, ", ")
}

// leafFKAction says what the step did to the leaf.
type leafFKAction string

const (
	// leafFKFound: the leaf already had the validated foreign key.
	leafFKFound leafFKAction = "found"
	// leafFKValidated: the leaf had the foreign key NOT VALID, and the step
	// validated it.
	leafFKValidated leafFKAction = "validated"
	// leafFKAdded: the step added the foreign key NOT VALID, then validated
	// it.
	leafFKAdded leafFKAction = "added"
)

// leafFKResult is the leaf's foreign key once the step is done.
type leafFKResult struct {
	action        leafFKAction
	constraint    string
	constraintOID uint32
}

// leafFKStatementStarted is told each statement the step is about to run,
// with its progress operation. It may be nil.
type leafFKStatementStarted func(op progress.Operation, sql string)

// ensureLeafForeignKey brings one leaf partition to the requested foreign
// key: exactly one foreign key of the requested definition, validated, and
// attached to no parent's foreign key. It reads the leaf and decides every
// call, so a rerun resumes from whatever an earlier call left:
//
//   - one validated foreign key of the definition, under any name, is the
//     result as it stands;
//   - one that is NOT VALID, under any name, is validated;
//   - otherwise the foreign key is added NOT VALID under the derived name,
//     then validated.
//
// Two of them, one already attached to a parent's foreign key, and a
// derived name another constraint holds are refused, typed, before anything
// is created; nothing the step did not create is dropped or renamed. The
// ADD and the VALIDATE each decide again under the leaf's lock, so a foreign
// key that lands on the leaf between the steps is found or refused, never
// left beside the step's. Each VALIDATE is bounded by b. The caller holds
// the parent's advisory table lock (LK-1).
func ensureLeafForeignKey(ctx context.Context, pool *pgxpool.Pool, fk resolvedFK, node partitionNode, b ValidateBudget, started leafFKStatementStarted) (leafFKResult, error) {
	if err := fk.spec.validate(); err != nil {
		return leafFKResult{}, err
	}
	if err := b.validate(); err != nil {
		return leafFKResult{}, err
	}
	if node.relkind != "r" {
		return leafFKResult{}, fmt.Errorf("%w: partition %s is of kind %q, and the foreign key step takes a leaf partition",
			ErrInvariantViolation, node.qualified(), node.relkind)
	}
	tx := briefTx{pool: pool}
	name := derivedPartitionIndexName(fk.spec.name, node.oid)
	var plan leafFKPlan
	err := tx.read(ctx, "inspect the foreign keys of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var err error
		plan, err = readAndDecideLeafFK(ctx, q, fk, node, name)
		return err
	})
	if err != nil {
		return leafFKResult{}, err
	}
	added := false
	if plan.state == leafFKAddNeeded {
		addSQL := leafFKAddSQL(fk.spec, node, name)
		reportLeafFKStatement(started, progress.OperationBrief, addSQL)
		if plan, added, err = addLeafForeignKey(ctx, tx, fk, node, name, addSQL); err != nil {
			return leafFKResult{}, fmt.Errorf("add foreign key %s on partition %s: %w", name, node.qualified(), err)
		}
	}
	result := leafFKResult{constraint: plan.fk.name, constraintOID: plan.fk.oid}
	switch plan.state {
	case leafFKDone:
		result.action = leafFKFound
		return result, nil
	case leafFKValidatePending:
		result.action = leafFKValidated
		if added {
			result.action = leafFKAdded
		}
	default:
		return leafFKResult{}, fmt.Errorf("%w: unknown leaf foreign key state %d", ErrInvariantViolation, plan.state)
	}
	if err := validateLeafForeignKey(ctx, pool, fk, node, result, b, started); err != nil {
		return leafFKResult{}, err
	}
	return result, nil
}

// reportLeafFKStatement tells started, when set, that a statement is about
// to run.
func reportLeafFKStatement(started leafFKStatementStarted, op progress.Operation, sql string) {
	if started != nil {
		started(op, sql)
	}
}

// leafFKAddSQL is the leaf's ADD FOREIGN KEY ... NOT VALID under the derived
// name.
func leafFKAddSQL(spec partitionedFKSpec, node partitionNode, name string) string {
	return "ALTER TABLE " + pgx.Identifier{node.schema, node.name}.Sanitize() +
		" ADD CONSTRAINT " + pgx.Identifier{name}.Sanitize() + " " + spec.clause() + " NOT VALID"
}

// leafFKValidateSQL is the leaf's VALIDATE CONSTRAINT of its foreign key.
func leafFKValidateSQL(node partitionNode, constraint string) string {
	return "ALTER TABLE " + pgx.Identifier{node.schema, node.name}.Sanitize() +
		" VALIDATE CONSTRAINT " + pgx.Identifier{constraint}.Sanitize()
}

// addLeafForeignKey runs the leaf's ADD FOREIGN KEY ... NOT VALID in one
// brief transaction and returns the decision it ends on, and whether it ran
// the ADD. Under the leaf's SHARE UPDATE EXCLUSIVE, which excludes every
// other constraint change on the leaf, it decides again: a foreign key of
// the definition that landed since the first read is taken as it stands,
// and nothing is added. The statement then takes SHARE ROW EXCLUSIVE on the
// leaf and, after it, on the referenced table, the order the server's own
// ADD FOREIGN KEY takes them, so a writer that holds the leaf and then
// touches the referenced table waits only for this brief transaction. It
// scans nothing. It reads back, before the commit, that the leaf has exactly
// one foreign key of the requested definition, NOT VALID under the derived
// name and adopted by nothing; the definition includes the referenced
// table's OID, so a name that resolves to another table rolls the ADD back.
// A rerun after an unknown commit outcome finds the foreign key and
// validates it.
func addLeafForeignKey(ctx context.Context, tx briefTx, fk resolvedFK, node partitionNode, name, addSQL string) (leafFKPlan, bool, error) {
	var plan leafFKPlan
	added := false
	err := tx.run(ctx, "add the foreign key of partition "+node.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// The partition stays in the tree, under its name, and its
			// constraints stay as read, until the transaction ends
			// (INV: ST-7).
			if err := lockAndProveNode(ctx, tx, node); err != nil {
				return err
			}
			var err error
			if plan, err = readAndDecideLeafFK(ctx, tx, fk, node, name); err != nil {
				return err
			}
			if plan.state != leafFKAddNeeded {
				return nil
			}
			// INV: LK-2 — the statement changes only the catalog: NOT VALID
			// skips the scan, and the locks it takes are held only for this
			// brief transaction under the pool's lock_timeout and
			// statement_timeout.
			if _, err := tx.Exec(ctx, addSQL); err != nil {
				return err
			}
			added = true
			if plan, err = readAndDecideLeafFK(ctx, tx, fk, node, name); err != nil {
				return err
			}
			return requireLeafFKPlan(node, plan, leafFKValidatePending, name, 0)
		})
	return plan, added, err
}

// readAndDecideLeafFK reads the leaf and decides from that one snapshot.
func readAndDecideLeafFK(ctx context.Context, q querier, fk resolvedFK, node partitionNode, name string) (leafFKPlan, error) {
	facts, err := readLeafFKFacts(ctx, q, fk, node, name)
	if err != nil {
		return leafFKPlan{}, err
	}
	return decideLeafForeignKey(node, name, fk, facts)
}

// requireLeafFKPlan requires the decision to be want on the constraint
// named name, and, when oid is nonzero, on the constraint with that OID.
func requireLeafFKPlan(node partitionNode, plan leafFKPlan, want leafFKState, name string, oid uint32) error {
	if plan.state != want {
		return fmt.Errorf("%w: ST-7: foreign key %s on partition %s is in state %d, want %d",
			ErrInvariantViolation, name, node.qualified(), plan.state, want)
	}
	if plan.fk.name != name {
		return fmt.Errorf("%w: ST-7: the foreign key of the requested definition on partition %s is %s, want %s",
			ErrInvariantViolation, node.qualified(), plan.fk.name, name)
	}
	if oid != 0 && plan.fk.oid != oid {
		return fmt.Errorf("%w: ST-7: foreign key %s on partition %s is no longer the constraint %d the step decided on",
			ErrInvariantViolation, name, node.qualified(), oid)
	}
	return nil
}

// validateLeafForeignKey runs the leaf's VALIDATE CONSTRAINT under b: the
// lock wait bounded by b.LockTimeout and the scan by b.Overall. VALIDATE
// takes SHARE UPDATE EXCLUSIVE on the leaf, which lets reads and writes go
// on, and ROW SHARE on the referenced table. Under that lock and before the
// statement, it proves the leaf is still in the tree and decides again: the
// leaf must still have exactly one foreign key of the requested definition,
// the one decided on, NOT VALID and adopted by nothing. After the statement,
// before the commit, the same decision must find it validated. A failure
// because rows violate the foreign key is ErrPartitionedForeignKeyViolation.
func validateLeafForeignKey(ctx context.Context, pool *pgxpool.Pool, fk resolvedFK, node partitionNode, res leafFKResult, b ValidateBudget, started leafFKStatementStarted) error {
	validateSQL := leafFKValidateSQL(node, res.constraint)
	st, err := statement.ParseOne(validateSQL)
	if err != nil {
		return fmt.Errorf("parse the validation of foreign key %s on partition %s: %w", res.constraint, node.qualified(), err)
	}
	before := func(ctx context.Context, tx pgx.Tx) error {
		// INV: ST-7 — the lock VALIDATE takes, taken first, holds the
		// partition in the tree and its constraints unchanged until the
		// transaction ends.
		if err := lockAndProveNode(ctx, tx, node); err != nil {
			return err
		}
		return proveLeafFKState(ctx, tx, fk, node, res, leafFKValidatePending)
	}
	after := func(ctx context.Context, tx pgx.Tx) error {
		return proveLeafFKState(ctx, tx, fk, node, res, leafFKDone)
	}
	reportLeafFKStatement(started, progress.OperationValidate, validateSQL)
	start := time.Now()
	err = executeBoundedAttemptAs(ctx, pool, st, Budget{LockTimeout: b.LockTimeout, StatementTimeout: b.Overall},
		catalogReadPath(), "", before, after)
	if err = corroborateValidateCancel(ctx, err, b, time.Since(start)); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateForeignKeyViolation {
			return fmt.Errorf("%w: foreign key %s on partition %s stays NOT VALID and enforced for new rows: %w",
				ErrPartitionedForeignKeyViolation, res.constraint, node.qualified(), err)
		}
		return fmt.Errorf("validate foreign key %s on partition %s: %w", res.constraint, node.qualified(), err)
	}
	return nil
}

// proveLeafFKState decides again from the leaf and requires the decision to
// be want on the constraint the step decided on.
func proveLeafFKState(ctx context.Context, q querier, fk resolvedFK, node partitionNode, res leafFKResult, want leafFKState) error {
	name := derivedPartitionIndexName(fk.spec.name, node.oid)
	plan, err := readAndDecideLeafFK(ctx, q, fk, node, name)
	if err != nil {
		return err
	}
	return requireLeafFKPlan(node, plan, want, res.constraint, res.constraintOID)
}
