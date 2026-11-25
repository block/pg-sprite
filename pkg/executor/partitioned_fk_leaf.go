package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
// is created; nothing the step did not create is dropped or renamed. Each
// VALIDATE is bounded by b. The caller holds the parent's advisory table
// lock (LK-1).
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
	var facts leafFKFacts
	err := tx.read(ctx, "inspect the foreign keys of partition "+node.qualified(), func(ctx context.Context, q querier) error {
		var err error
		facts, err = readLeafFKFacts(ctx, q, fk, node, name)
		return err
	})
	if err != nil {
		return leafFKResult{}, err
	}
	plan, err := decideLeafForeignKey(node, name, fk, facts)
	if err != nil {
		return leafFKResult{}, err
	}
	result := leafFKResult{constraint: plan.fk.name, constraintOID: plan.fk.oid}
	switch plan.state {
	case leafFKDone:
		result.action = leafFKFound
		return result, nil
	case leafFKValidatePending:
		result.action = leafFKValidated
	case leafFKAddNeeded:
		result.action = leafFKAdded
		result.constraint = name
		addSQL := leafFKAddSQL(fk.spec, node, name)
		reportLeafFKStatement(started, progress.OperationBrief, addSQL)
		if result.constraintOID, err = addLeafForeignKey(ctx, tx, fk, node, name, addSQL); err != nil {
			return leafFKResult{}, err
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

// lockAndProveReference locks the referenced table in SHARE ROW EXCLUSIVE,
// the lock ADD FOREIGN KEY takes on it anyway, and proves under it that its
// name still resolves to the table the build resolved. A rename needs ACCESS
// EXCLUSIVE, which the lock excludes until the transaction ends, so the
// statement that names the table binds the one proved here (INV: ST-7).
func lockAndProveReference(ctx context.Context, tx pgx.Tx, ref fkReference) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{ref.schema, ref.table}.Sanitize()+" IN SHARE ROW EXCLUSIVE MODE")
	if err != nil {
		return err
	}
	current, found, err := relationByName(ctx, tx, ref.schema, ref.table)
	if err != nil {
		return err
	}
	if !found || current.oid != ref.oid {
		return fmt.Errorf("%w: ST-7: %s no longer names the referenced table %d the build resolved",
			ErrInvariantViolation, ref.qualified(), ref.oid)
	}
	return nil
}

// addLeafForeignKey runs the leaf's ADD FOREIGN KEY ... NOT VALID in one
// brief transaction and returns the constraint's OID. The statement takes
// SHARE ROW EXCLUSIVE on the leaf and on the referenced table until the
// transaction ends and scans nothing. It reads back, before the commit, that
// it made a NOT VALID foreign key of the requested definition under the
// derived name, adopted by nothing; any other outcome rolls it back. A rerun
// after an unknown commit outcome finds the foreign key and validates it.
func addLeafForeignKey(ctx context.Context, tx briefTx, fk resolvedFK, node partitionNode, name, addSQL string) (uint32, error) {
	var constraintOID uint32
	err := tx.run(ctx, "add the foreign key of partition "+node.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// The partition stays in the tree, under its name, until the
			// transaction ends (INV: ST-7).
			if err := lockAndProveNode(ctx, tx, node); err != nil {
				return err
			}
			if err := lockAndProveReference(ctx, tx, fk.ref); err != nil {
				return err
			}
			// INV: LK-2 — the statement changes only the catalog: NOT VALID
			// skips the scan, and the lock it takes is held only for this
			// brief transaction under the pool's lock_timeout and
			// statement_timeout.
			if _, err := tx.Exec(ctx, addSQL); err != nil {
				return err
			}
			added, err := leafFKByName(ctx, tx, fk, node, name)
			if err != nil {
				return err
			}
			if added.validated || added.parentOID != 0 {
				return fmt.Errorf("%w: ST-7: after ADD FOREIGN KEY ... NOT VALID, foreign key %s on partition %s is validated or adopted",
					ErrInvariantViolation, name, node.qualified())
			}
			constraintOID = added.oid
			return nil
		})
	return constraintOID, err
}

// leafFKByName reads the leaf's foreign key under name and requires it to
// have the requested definition.
func leafFKByName(ctx context.Context, q querier, fk resolvedFK, node partitionNode, name string) (catalogFK, error) {
	attnums, err := fkColumnAttnums(ctx, q, node.oid, node.qualified(), fk.spec.columns)
	if err != nil {
		return catalogFK{}, err
	}
	fks, err := readForeignKeys(ctx, q, node.oid, node.qualified(), fk.serverMajor)
	if err != nil {
		return catalogFK{}, err
	}
	for _, k := range fks {
		if k.name != name {
			continue
		}
		if !k.isRequested(fk.spec, attnums, fk.ref) {
			return catalogFK{}, fmt.Errorf("%w: ST-7: foreign key %s on partition %s does not have the requested definition",
				ErrInvariantViolation, name, node.qualified())
		}
		return k, nil
	}
	return catalogFK{}, fmt.Errorf("%w: ST-7: partition %s has no foreign key %s", ErrInvariantViolation, node.qualified(), name)
}

// validateLeafForeignKey runs the leaf's VALIDATE CONSTRAINT under b: the
// lock wait bounded by b.LockTimeout and the scan by b.Overall. VALIDATE
// takes SHARE UPDATE EXCLUSIVE on the leaf, which lets reads and writes go
// on, and ROW SHARE on the referenced table. Under that lock and before the
// statement, it proves the leaf is still in the tree and that the
// constraint under the name is still the one decided on, of the requested
// definition and adopted by nothing; after the statement, before the
// commit, it reads back that it is validated.
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
		return proveLeafFKState(ctx, tx, fk, node, res, false)
	}
	after := func(ctx context.Context, tx pgx.Tx) error {
		return proveLeafFKState(ctx, tx, fk, node, res, true)
	}
	reportLeafFKStatement(started, progress.OperationValidate, validateSQL)
	start := time.Now()
	err = executeBoundedAttemptAs(ctx, pool, st, Budget{LockTimeout: b.LockTimeout, StatementTimeout: b.Overall},
		catalogReadPath(), "", before, after)
	if err = corroborateValidateCancel(ctx, err, b, time.Since(start)); err != nil {
		return fmt.Errorf("validate foreign key %s on partition %s: %w", res.constraint, node.qualified(), err)
	}
	return nil
}

// proveLeafFKState proves the constraint under the step's name is still the
// one the step decided on, of the requested definition and adopted by
// nothing, and that its convalidated is validated.
func proveLeafFKState(ctx context.Context, q querier, fk resolvedFK, node partitionNode, res leafFKResult, validated bool) error {
	current, err := leafFKByName(ctx, q, fk, node, res.constraint)
	if err != nil {
		return err
	}
	if current.oid != res.constraintOID || current.parentOID != 0 {
		return fmt.Errorf("%w: ST-7: foreign key %s on partition %s is no longer the constraint %d the step decided on, adopted by nothing",
			ErrInvariantViolation, res.constraint, node.qualified(), res.constraintOID)
	}
	if current.validated != validated {
		return fmt.Errorf("%w: foreign key %s on partition %s has convalidated %t, want %t",
			ErrInvariantViolation, res.constraint, node.qualified(), current.validated, validated)
	}
	return nil
}
