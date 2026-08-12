package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrNotNativeSafe is returned when a statement is not one of the native
// forms whose lock and rewrite behavior the executor can prove locally.
var ErrNotNativeSafe = errors.New("statement is not a proven native-safe form")

// ErrBackingIndexNotProven is returned when the execution-time catalog
// inspection cannot prove the backing index named by ADD CONSTRAINT ...
// USING INDEX is safe to attach. It is a refusal, not an operational
// failure: nothing was executed against the target table.
var ErrBackingIndexNotProven = errors.New("the backing index cannot be proven safe to attach")

// NativeStepKind identifies how one admitted native statement is executed.
type NativeStepKind int

const (
	// NativeStepDirect is a catalog-only operation, a PG11+ fast-default
	// column addition, or one of the cheap constraint idiom steps. It runs
	// in its own transaction under the direct-step budget.
	NativeStepDirect NativeStepKind = iota + 1
	// NativeStepConcurrentIndex is a verified concurrent index build.
	NativeStepConcurrentIndex
	// NativeStepValidateConstraint is VALIDATE CONSTRAINT: a long
	// SHARE UPDATE EXCLUSIVE scan that gets the overall-deadline wait
	// policy instead of the blanket per-lock timeout (invariant LK-2's
	// exception list).
	NativeStepValidateConstraint
	// NativeStepUsingIndex is ADD CONSTRAINT ... PRIMARY KEY/UNIQUE
	// USING INDEX: a direct step that first proves the backing index safe
	// to attach by inspecting the catalog at execution time.
	NativeStepUsingIndex
)

// NativeStep is one parsed, admitted statement in an executable sequence.
// Values can only be constructed by PrepareNativeSequence.
type NativeStep struct {
	sql  string
	kind NativeStepKind
	// usingIndex carries the identities the execution-time catalog proof
	// needs; set only for NativeStepUsingIndex.
	usingIndex usingIndexStep
}

// usingIndexStep names what a USING INDEX attachment must prove: the
// backing index, resolved in the target table's schema (PostgreSQL requires
// the index to live in the same schema as its table), attached to exactly
// this table.
type usingIndexStep struct {
	schema     string
	table      string
	index      string
	primaryKey bool
}

// SQL returns the exact statement that was admitted.
func (s NativeStep) SQL() string { return s.sql }

// Kind returns the execution path for the statement.
func (s NativeStep) Kind() NativeStepKind { return s.kind }

// PrepareNativeSequence parses and independently admits every statement in
// sql. The planner's classification is intentionally not accepted as proof:
// this safety-critical boundary recognizes only the native forms it executes
// itself and fails closed on everything else. Every statically provable
// refusal — statement shape, index naming, schema qualification — happens
// here, before anything executes, so a sequence that admission can refuse
// never leaves partial progress behind.
func PrepareNativeSequence(sql []string) ([]NativeStep, error) {
	if len(sql) == 0 {
		return nil, fmt.Errorf("%w: sequence is empty", ErrNotNativeSafe)
	}
	steps := make([]NativeStep, 0, len(sql))
	for i, text := range sql {
		step, err := admitNativeStep(text)
		if err != nil {
			return nil, fmt.Errorf("admit native step %d: %w", i+1, err)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// ExecuteNativeSequence executes an already-safe native sequence one step at
// a time. Each direct step runs in its own transaction under the direct
// budget; concurrent index builds and constraint validations run under the
// overall-deadline wait policy. Partial progress is deliberately visible to
// the caller: a later failure never rolls back an earlier safe step.
func ExecuteNativeSequence(ctx context.Context, pool *pgxpool.Pool, sql []string, direct Budget, concurrent ConcurrentBudget) error {
	if err := direct.validate(); err != nil {
		return err
	}
	if err := concurrent.validate(); err != nil {
		return err
	}
	steps, err := PrepareNativeSequence(sql)
	if err != nil {
		return err
	}
	for i, step := range steps {
		var stepErr error
		switch step.kind {
		case NativeStepConcurrentIndex:
			_, stepErr = BuildIndexConcurrently(ctx, pool, step.sql, concurrent)
		case NativeStepValidateConstraint:
			stepErr = execValidateStep(ctx, pool, step.sql, concurrent)
		case NativeStepUsingIndex:
			stepErr = execUsingIndexStep(ctx, pool, step, direct)
		case NativeStepDirect:
			stepErr = execDirectStep(ctx, pool, step.sql, direct)
		default:
			// INV: ST-7 — a step kind admission never produces means the
			// statement was not gated; executing it would run ungated SQL.
			return fmt.Errorf("%w: ST-7: native step kind %d was not produced by admission", ErrInvariantViolation, step.kind)
		}
		if stepErr != nil {
			return fmt.Errorf("execute native step %d: %w", i+1, stepErr)
		}
	}
	return nil
}

func admitNativeStep(sql string) (NativeStep, error) {
	st, err := statement.ParseOne(sql)
	if err != nil {
		return NativeStep{}, err
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return NativeStep{}, err
	}
	if len(ops) != 1 {
		return NativeStep{}, fmt.Errorf("%w: expected one operation, got %d", ErrNotNativeSafe, len(ops))
	}
	op := ops[0]
	if st.Kind() == statement.KindCreateIndex && op.Concurrent {
		// A concurrent build takes only SHARE UPDATE EXCLUSIVE, unique or
		// not; BuildIndexConcurrently performs the target-identity and
		// invalid-index checks that need a database.
		if err := admitConcurrentIndexShape(op, st.Schema()); err != nil {
			return NativeStep{}, err
		}
		return NativeStep{sql: sql, kind: NativeStepConcurrentIndex}, nil
	}
	if op.Kind == statement.OpValidateConstraint && op.Name != "" {
		return NativeStep{sql: sql, kind: NativeStepValidateConstraint}, nil
	}
	if op.Kind == statement.OpAddConstraint && op.IndexName != "" {
		return admitUsingIndexStep(sql, st, op)
	}
	if directNativeOp(op) {
		return NativeStep{sql: sql, kind: NativeStepDirect}, nil
	}
	return NativeStep{}, fmt.Errorf("%w: %s", ErrNotNativeSafe, op.Describe())
}

// admitUsingIndexStep admits ADD CONSTRAINT ... USING INDEX: only PRIMARY
// KEY and UNIQUE can consume a prebuilt index, and the table must be
// schema-qualified so the execution-time proof resolves the same index the
// statement will, independent of search_path.
func admitUsingIndexStep(sql string, st statement.Statement, op statement.Op) (NativeStep, error) {
	if op.Constraint != statement.ConstraintPrimaryKey && op.Constraint != statement.ConstraintUnique {
		return NativeStep{}, fmt.Errorf("%w: only PRIMARY KEY and UNIQUE constraints can consume a prebuilt index", ErrNotNativeSafe)
	}
	if st.Schema() == "" {
		return NativeStep{}, fmt.Errorf("%w: USING INDEX needs a schema-qualified table so the backing index resolves independent of search_path", ErrNotNativeSafe)
	}
	return NativeStep{
		sql:  sql,
		kind: NativeStepUsingIndex,
		usingIndex: usingIndexStep{
			schema:     st.Schema(),
			table:      st.Table(),
			index:      op.IndexName,
			primaryKey: op.Constraint == statement.ConstraintPrimaryKey,
		},
	}, nil
}

func directNativeOp(op statement.Op) bool {
	switch op.Kind {
	case statement.OpCreateTable:
		return !op.PartitionOf
	case statement.OpAddColumn:
		// A stored generated column is computed for every existing row: a
		// full table rewrite under ACCESS EXCLUSIVE, never a direct step.
		return op.Default != statement.DefaultExpression && len(op.InlineConstraints) == 0 && !op.GeneratedStored
	case statement.OpDropColumn, statement.OpSetDefault, statement.OpDropDefault,
		statement.OpDropNotNull, statement.OpSetColumnOptions, statement.OpRenameIndex,
		statement.OpSetSchema, statement.OpDropConstraint:
		return true
	case statement.OpAddConstraint:
		if op.NotValid {
			return op.Constraint == statement.ConstraintCheck || op.Constraint == statement.ConstraintForeignKey
		}
		return false
	case statement.OpSetNotNull:
		// SET NOT NULL scans the whole table under ACCESS EXCLUSIVE unless
		// a validated CHECK (col IS NOT NULL) already proves the invariant
		// — and proving that constraint exists takes execution-time
		// catalog evidence this executor does not collect yet. Fails
		// closed until it does.
		return false
	default:
		return false
	}
}

// execDirectStep runs one admitted direct statement in its own transaction.
// The budgets are applied with SET LOCAL regardless of the pool's session
// defaults; a budget overrun is cancelled by the server, rolls back, and
// surfaces as a typed *BudgetError.
func execDirectStep(ctx context.Context, pool *pgxpool.Pool, sql string, b Budget) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin direct step: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path a rollback error
		// only means the connection died, and the server aborts the
		// transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := setLocalStepBudgets(ctx, tx, b); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		if budgetErr := asBudgetError(err, b); budgetErr != nil {
			return budgetErr
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit direct step: %w", err)
	}
	return nil
}

// setLocalStepBudgets applies the direct budget inside tx.
//
// INV: LK-2 — every strong-lock acquisition in this path is bounded by
// construction: the budgets are applied inside this transaction, so the
// step cannot sit at the head of the lock queue or run a surprise rewrite
// past them even on a misconfigured pool. A bare integer is milliseconds to
// PostgreSQL; SET LOCAL cannot use bind parameters.
func setLocalStepBudgets(ctx context.Context, tx pgx.Tx, b Budget) error {
	setBudgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, setBudgets); err != nil {
		return fmt.Errorf("set step budgets: %w", err)
	}
	return nil
}

// execValidateStep runs VALIDATE CONSTRAINT under the same wait policy as a
// concurrent build.
//
// INV: LK-2 — the exception policy: VALIDATE CONSTRAINT's scan legitimately
// outlives any per-statement budget sized for catalog flips, and its lock
// waits must not be cancelled by a blanket lock_timeout, so it runs with
// lock_timeout disabled under one overall deadline. That is safe with
// respect to the lock queue: a waiting SHARE UPDATE EXCLUSIVE request does
// not block the normal reads and writes queued behind it.
func execValidateStep(ctx context.Context, pool *pgxpool.Pool, sql string, b ConcurrentBudget) error {
	conn, release, err := acquireBudgetedSession(ctx, pool, b)
	if err != nil {
		return err
	}
	defer release()
	start := time.Now()
	_, execErr := conn.Exec(ctx, sql)
	if execErr == nil {
		return nil
	}
	if typed := typeOverallBudgetError(execErr, b, time.Since(start)); typed != nil {
		return typed
	}
	return execErr
}

// execUsingIndexStep attaches a prebuilt index as a PRIMARY KEY or UNIQUE
// constraint in one transaction: take the target table's ACCESS EXCLUSIVE
// lock under the lock budget, run the catalog proof under that lock, then
// execute the admitted ALTER. Locking before the proof leaves no window for
// another session to drop NOT NULL or swap the index between proof and
// attachment — the ALTER needs the same lock anyway, so holding it through
// the proof adds one catalog query, not a new wait.
func execUsingIndexStep(ctx context.Context, pool *pgxpool.Pool, step NativeStep, b Budget) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin using-index step: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path a rollback error
		// only means the connection died, and the server aborts the
		// transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := setLocalStepBudgets(ctx, tx, b); err != nil {
		return err
	}
	table := pgx.Identifier{step.usingIndex.schema, step.usingIndex.table}.Sanitize()
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		if budgetErr := asBudgetError(err, b); budgetErr != nil {
			return budgetErr
		}
		return fmt.Errorf("lock table %s for constraint attachment: %w", table, err)
	}
	if err := proveBackingIndex(ctx, tx, step.usingIndex); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, step.sql); err != nil {
		if budgetErr := asBudgetError(err, b); budgetErr != nil {
			return budgetErr
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit using-index step: %w", err)
	}
	return nil
}

// proveBackingIndexSQL resolves the backing index by schema and name and
// reports every property the attachment proof needs in one snapshot. Key
// columns are the first indnkeyatts entries of indkey, selected by
// ordinality because int2vector subscripts start at zero; an expression key
// (attnum 0) has no pg_attribute row, so keys_not_null covers plain columns
// only — the separate expressional refusal keeps that sound. Every catalog
// relation, function, and operator is pg_catalog-qualified: search_path may
// list a user schema first, where an impostor to_regclass — or a user
// operator named = — would silently falsify the proof.
const proveBackingIndexSQL = `
SELECT x.indisvalid,
       x.indisready,
       x.indisunique,
       x.indpred IS NOT NULL,
       x.indexprs IS NOT NULL,
       COALESCE(x.indrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($3), false),
       COALESCE((SELECT pg_catalog.bool_and(a.attnotnull)
                   FROM pg_catalog.unnest(x.indkey) WITH ORDINALITY AS k(attnum, ord)
                   JOIN pg_catalog.pg_attribute a
                     ON a.attrelid OPERATOR(pg_catalog.=) x.indrelid
                    AND a.attnum OPERATOR(pg_catalog.=) k.attnum
                  WHERE k.ord OPERATOR(pg_catalog.<=) x.indnkeyatts), false)
  FROM pg_catalog.pg_index x
  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) x.indexrelid
  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
 WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`

// proveBackingIndex is the execution-time admission for a USING INDEX
// attachment. The dangerous case is the silent one: attaching a PRIMARY KEY
// over a nullable key column makes PostgreSQL run an implicit SET NOT NULL
// scan of the whole heap under ACCESS EXCLUSIVE — so a PK attachment is
// admitted only when every key column is already NOT NULL. The proof runs
// on q — the transaction already holding the target table's ACCESS
// EXCLUSIVE lock — so what it proves stays true through the attachment.
func proveBackingIndex(ctx context.Context, q querier, u usingIndexStep) error {
	index := pgx.Identifier{u.schema, u.index}.Sanitize()
	table := pgx.Identifier{u.schema, u.table}.Sanitize()
	var valid, ready, unique, partial, expressional, onTarget, keysNotNull bool
	err := q.QueryRow(ctx, proveBackingIndexSQL, u.schema, u.index, table).
		Scan(&valid, &ready, &unique, &partial, &expressional, &onTarget, &keysNotNull)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: index %s does not exist", ErrBackingIndexNotProven, index)
	}
	if err != nil {
		return fmt.Errorf("inspect backing index %s: %w", index, err)
	}
	switch {
	case !onTarget:
		return fmt.Errorf("%w: index %s is not an index on table %s", ErrBackingIndexNotProven, index, table)
	case !valid:
		return fmt.Errorf("%w: index %s is invalid — a failed concurrent build cannot back a constraint", ErrBackingIndexNotProven, index)
	case !ready:
		return fmt.Errorf("%w: index %s is not ready for inserts — an in-progress build cannot back a constraint", ErrBackingIndexNotProven, index)
	case !unique:
		return fmt.Errorf("%w: index %s is not unique", ErrBackingIndexNotProven, index)
	case partial:
		return fmt.Errorf("%w: index %s is partial and cannot back a table-wide constraint", ErrBackingIndexNotProven, index)
	case expressional:
		return fmt.Errorf("%w: index %s indexes expressions, not plain columns", ErrBackingIndexNotProven, index)
	case u.primaryKey && !keysNotNull:
		return fmt.Errorf("%w: index %s has a nullable key column — attaching it as PRIMARY KEY would run an implicit full-table SET NOT NULL scan under ACCESS EXCLUSIVE; make the column NOT NULL first", ErrBackingIndexNotProven, index)
	}
	return nil
}
