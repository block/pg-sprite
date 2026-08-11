package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrNotNativeSafe is returned when a statement is not one of the native
// forms whose lock and rewrite behavior the executor can prove locally.
var ErrNotNativeSafe = errors.New("statement is not a proven native-safe form")

// NativeStepKind identifies how one admitted native statement is executed.
type NativeStepKind int

const (
	// NativeStepDirect is a catalog-only operation, a PG11+ fast-default
	// column addition, or one of the cheap constraint idiom steps.
	NativeStepDirect NativeStepKind = iota + 1
	// NativeStepConcurrentIndex is a verified concurrent unique-index build.
	NativeStepConcurrentIndex
)

// NativeStep is one parsed, admitted statement in an executable sequence.
// Values can only be constructed by PrepareNativeSequence.
type NativeStep struct {
	sql  string
	kind NativeStepKind
}

// SQL returns the exact statement that was admitted.
func (s NativeStep) SQL() string { return s.sql }

// Kind returns the execution path for the statement.
func (s NativeStep) Kind() NativeStepKind { return s.kind }

// PrepareNativeSequence parses and independently admits every statement in
// sql. The planner's classification is intentionally not accepted as proof:
// this safety-critical boundary recognizes only the native forms it executes
// itself and fails closed on everything else.
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
// a time. Each direct step gets its own implicit transaction; concurrent
// index builds use the verified executor and therefore also run outside an
// enclosing transaction. Partial progress is deliberately visible to the
// caller: a later failure never rolls back an earlier safe step.
func ExecuteNativeSequence(ctx context.Context, pool *pgxpool.Pool, sql []string, concurrentBudget ConcurrentBudget) error {
	steps, err := PrepareNativeSequence(sql)
	if err != nil {
		return err
	}
	for i, step := range steps {
		switch step.kind {
		case NativeStepConcurrentIndex:
			if _, err := BuildIndexConcurrently(ctx, pool, step.sql, concurrentBudget); err != nil {
				return fmt.Errorf("execute native step %d: %w", i+1, err)
			}
		case NativeStepDirect:
			if _, err := pool.Exec(ctx, step.sql); err != nil {
				return fmt.Errorf("execute native step %d: %w", i+1, err)
			}
		default:
			return fmt.Errorf("%w: unknown native step kind %d", ErrInvariantViolation, step.kind)
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
	if st.Kind() == statement.KindCreateIndex && op.Concurrent && op.Unique {
		// BuildIndexConcurrently performs the stronger name, qualification,
		// IF NOT EXISTS, target-identity, and invalid-index checks.
		return NativeStep{sql: sql, kind: NativeStepConcurrentIndex}, nil
	}
	if directNativeOp(op) {
		return NativeStep{sql: sql, kind: NativeStepDirect}, nil
	}
	return NativeStep{}, fmt.Errorf("%w: %s", ErrNotNativeSafe, op.Describe())
}

func directNativeOp(op statement.Op) bool {
	switch op.Kind {
	case statement.OpCreateTable:
		return !op.PartitionOf
	case statement.OpAddColumn:
		return op.Default != statement.DefaultExpression && len(op.InlineConstraints) == 0
	case statement.OpDropColumn, statement.OpSetDefault, statement.OpDropDefault,
		statement.OpDropNotNull, statement.OpSetColumnOptions, statement.OpRenameIndex,
		statement.OpSetSchema, statement.OpDropConstraint:
		return true
	case statement.OpAddConstraint:
		if op.NotValid {
			return op.Constraint == statement.ConstraintCheck || op.Constraint == statement.ConstraintForeignKey
		}
		return op.UsingIndex && op.Constraint == statement.ConstraintPrimaryKey
	case statement.OpValidateConstraint:
		return op.Name != ""
	default:
		return false
	}
}
