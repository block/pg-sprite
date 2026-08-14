// This file is the native sequence executor: it runs a planner-produced
// safer native sequence under the autocommit-each-step contract
// (planner.ExecutionAutocommit) — one statement at a time, in order, each
// in its own implicit or bounded transaction, never inside one enclosing
// block. It is what executes the Phase 3 idioms whose safety comes from
// their sequencing: NOT VALID plus an online VALIDATE, ADD PRIMARY KEY /
// UNIQUE USING INDEX over a concurrent build, the four-step SET NOT NULL,
// and the single-step fast-default and metadata-only changes.
//
// The executor never trusts the caller's classification (see SAFETY.md):
// every step is re-parsed by the real grammar and admitted by shape before
// anything executes, and every step runs bounded — brief catalog steps
// under the optimistic attempt's budgets, the constraint-validation scan
// under its own generous budget, and concurrent index builds under the
// dedicated CONCURRENTLY executor. A step that turns out to do rewrite
// work is cancelled cleanly by its budget, exactly like a blind optimistic
// attempt.
//
// A failed step ends the run immediately: the steps before it committed
// and their partial state remains, by design — each planner sequence
// constructor documents what a failed step leaves behind and how a retry
// resumes. The typed *SequenceStepError names the failed step so an
// operator or orchestrator can apply that contract.

package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// Typed admission refusals for the sequence executor. Admission covers the
// whole sequence before the first step executes, so a sequence that cannot
// be finished is never started.
var (
	// ErrEmptySequence is returned for a sequence with no steps: reporting
	// success over nothing executed would be a false proof.
	ErrEmptySequence = errors.New("sequence has no steps")
	// ErrUnsupportedSequenceStep is returned when a step is not one of the
	// shapes this executor can run safely: an ALTER TABLE step, or a
	// CREATE INDEX CONCURRENTLY step. The concurrent forms of other
	// statements are refused deliberately — a DROP INDEX CONCURRENTLY or
	// REINDEX CONCURRENTLY is not driven yet, and a cancelled
	// DETACH PARTITION CONCURRENTLY leaves a detach-pending partition
	// state this executor does not own detecting or recovering.
	ErrUnsupportedSequenceStep = errors.New("step is not a shape the sequence executor can run safely")
)

// StepKind is the typed execution class a step was admitted under;
// automation branches on it, never on the step's SQL text.
type StepKind string

// The execution classes a step can be admitted under.
const (
	// StepBrief: a bounded transactional run under the brief budgets — a
	// catalog change that must prove itself effectively instant, exactly
	// like an optimistic attempt.
	StepBrief StepKind = "brief"
	// StepConcurrentIndexBuild: a CREATE INDEX CONCURRENTLY delegated to
	// the dedicated concurrent build executor, with its wait policy and
	// invalid-index verdict.
	StepConcurrentIndexBuild StepKind = "concurrent-index-build"
	// StepValidateConstraint: an ALTER TABLE ... VALIDATE CONSTRAINT — a
	// long online scan under SHARE UPDATE EXCLUSIVE, bounded by the
	// validate budget rather than the brief one.
	StepValidateConstraint StepKind = "validate-constraint"
)

// ValidateBudget bounds one VALIDATE CONSTRAINT step. The validation scan
// is long by design — it is the online half of the NOT VALID pattern — so
// it gets its own overall bound instead of the brief statement budget,
// while lock acquisition stays tightly bounded: the SHARE UPDATE EXCLUSIVE
// it takes conflicts with other DDL, and queueing behind one must not
// stall the sequence for the whole scan budget.
type ValidateBudget struct {
	// LockTimeout bounds how long the step may wait in the lock queue.
	LockTimeout time.Duration
	// Overall bounds the whole validation scan via statement_timeout;
	// expect large tables to need a generous value.
	Overall time.Duration
}

// validate rejects budgets that would leave the validation unbounded.
func (b ValidateBudget) validate() error {
	// INV: LK-2 — the validation is bounded by construction; below one
	// millisecond a setting truncates to zero, which disables the
	// corresponding PostgreSQL limit entirely.
	if b.LockTimeout < minBudget {
		return fmt.Errorf("validate lock budget must be at least %s, got %s", minBudget, b.LockTimeout)
	}
	if b.LockTimeout > maxOverallBudget {
		return fmt.Errorf("validate lock budget must be at most %s, got %s", maxOverallBudget, b.LockTimeout)
	}
	if b.Overall < minBudget {
		return fmt.Errorf("validate overall budget must be at least %s, got %s", minBudget, b.Overall)
	}
	if b.Overall > maxOverallBudget {
		return fmt.Errorf("validate overall budget must be at most %s, got %s", maxOverallBudget, b.Overall)
	}
	return nil
}

// SequenceBudget bounds one sequence run: each admitted step class carries
// its own budget, because the classes have opposite needs — a brief
// catalog step must be cancelled fast, a validation scan and a concurrent
// build must be allowed to run long.
type SequenceBudget struct {
	// Brief bounds every brief catalog step.
	Brief Budget
	// Concurrent bounds every concurrent index build step.
	Concurrent ConcurrentBudget
	// Validate bounds every VALIDATE CONSTRAINT step.
	Validate ValidateBudget
}

// validate rejects budget sets with any unbounded member. All three are
// validated regardless of which step classes the sequence contains: a
// budget set is a unit, and admitting a partially-invalid one would make
// the same SequenceBudget pass or fail depending on the SQL next to it.
func (b SequenceBudget) validate() error {
	if err := b.Brief.validate(); err != nil {
		return err
	}
	if err := b.Concurrent.validate(); err != nil {
		return err
	}
	return b.Validate.validate()
}

// StepReport says what one committed step did, machine-readably.
type StepReport struct {
	// SQL is the step's statement as submitted.
	SQL string `json:"sql"`
	// Kind is the execution class the step ran under.
	Kind StepKind `json:"kind"`
	// Duration is the wall-clock time of the step, session setup and
	// verification included. It encodes as integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
	// Index carries the concurrent build's verified report; nil for every
	// other step kind.
	Index *IndexBuildReport `json:"index,omitempty"`
}

// SequenceReport is the record of a sequence run: one report per committed
// step, in execution order. On success it covers every step; alongside a
// *SequenceStepError it covers exactly the committed prefix, so a caller
// can disclose what already happened.
type SequenceReport struct {
	// Steps are the per-step reports, in execution order.
	Steps []StepReport `json:"steps"`
}

// SequenceStepError reports that a step failed and the run stopped there.
// The steps before it committed and their partial state remains — the
// planner's sequence constructors document what each failed step leaves
// behind and how a retry resumes. Err carries the step's own typed failure
// (*BudgetError, *InvalidIndexError, a server error) for errors.Is/As.
type SequenceStepError struct {
	// Step is the failed step's 1-based position, matching the numbering
	// the planner's partial-failure contracts use.
	Step int
	// Total is the number of steps the sequence was admitted with.
	Total int
	// Kind is the execution class the failed step ran under.
	Kind StepKind
	// SQL is the failed step's statement.
	SQL string
	// Err is the step's underlying failure.
	Err error
}

// Error implements the error interface. It names the failed step and the
// committed prefix, because "what already happened" is the first triage
// question a partial sequence raises.
func (e *SequenceStepError) Error() string {
	if e.Step == 1 {
		return fmt.Sprintf("sequence step 1 of %d (%s) failed; no earlier steps had committed: %v",
			e.Total, e.Kind, e.Err)
	}
	return fmt.Sprintf("sequence step %d of %d (%s) failed; steps before it committed and their state remains: %v",
		e.Step, e.Total, e.Kind, e.Err)
}

// Unwrap exposes the step's failure to errors.Is/As.
func (e *SequenceStepError) Unwrap() error { return e.Err }

// sequenceStep is one admitted step: its statement, re-parsed by the real
// grammar, and the execution class admission proved for it.
type sequenceStep struct {
	st   statement.Statement
	kind StepKind
}

// RunSequence runs steps in order against the preflighted table, each step
// in its own implicit or bounded transaction — the autocommit-each-step
// contract a planner-produced safer sequence carries. The pool must come
// from pkg/dbconn and, when the sequence contains a concurrent index build,
// must allow at least two connections (see BuildIndexConcurrently). The
// whole sequence is admitted before the first step executes: every step is
// re-parsed, its shape classified, and its target verified against the
// preflight proof, so a sequence this executor cannot finish is never
// started. On success every step committed and the report says what each
// did. On failure the run stops at the failing step and returns a typed
// *SequenceStepError; the committed prefix remains, per the planner's
// documented partial-failure contracts, and the returned report covers
// exactly that prefix.
//
// Like the concurrent build — and unlike a blind optimistic attempt — no
// size-guard proof is required beyond the preflight itself: long scans on
// large tables are the sequence pattern's purpose, and every brief step is
// still individually bounded by the brief budgets. retry bounds
// lock_timeout retries on each owner-gated step, exactly as in
// ExecuteNative.
func RunSequence(ctx context.Context, pool *pgxpool.Pool, pt preflight.PreflightedTable, steps []string, b SequenceBudget, retry RetryPolicy) (SequenceReport, error) {
	var rep SequenceReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	// A defective retry policy is decidable at admission; ExecuteNative
	// would refuse it anyway, but discovering that mid-run would leave a
	// committed prefix behind a refusal this executor could have made up
	// front.
	if err := retry.validate(); err != nil {
		return rep, err
	}
	admitted, err := admitSequence(pt.Schema(), pt.Table(), steps)
	if err != nil {
		return rep, err
	}
	// INV: LK-2 — the concurrent executor's pool guard is re-proven for
	// the whole sequence before the first step executes: a too-small pool
	// is decidable now, and letting BuildIndexConcurrently discover it
	// mid-run would leave a committed prefix behind a refusal this
	// executor could have made up front.
	if sequenceHasConcurrentBuild(admitted) && pool.Config().MaxConns < 2 {
		return rep, ErrPoolTooSmall
	}
	for i, step := range admitted {
		start := time.Now()
		var indexReport *IndexBuildReport
		switch step.kind {
		case StepConcurrentIndexBuild:
			r, buildErr := BuildIndexConcurrently(ctx, pool, step.st.SQL(), b.Concurrent)
			err = buildErr
			if buildErr == nil {
				indexReport = &r
			}
		case StepValidateConstraint:
			err = ExecuteNative(ctx, pool, pt, step.st, Budget{
				LockTimeout:      b.Validate.LockTimeout,
				StatementTimeout: b.Validate.Overall,
			}, retry)
			err = corroborateValidateCancel(err, b.Validate, time.Since(start))
		case StepBrief:
			err = ExecuteNative(ctx, pool, pt, step.st, b.Brief, retry)
		default:
			// Admission produces only the three kinds above; an unknown
			// kind here is a programming error and aborts fail-closed.
			err = fmt.Errorf("%w: unhandled step kind %q", ErrInvariantViolation, step.kind)
		}
		if err != nil {
			return rep, &SequenceStepError{Step: i + 1, Total: len(admitted), Kind: step.kind, SQL: step.st.SQL(), Err: err}
		}
		rep.Steps = append(rep.Steps, StepReport{
			SQL:      step.st.SQL(),
			Kind:     step.kind,
			Duration: time.Since(start),
			Index:    indexReport,
		})
	}
	return rep, nil
}

// sequenceHasConcurrentBuild reports whether any admitted step is a
// concurrent index build — the class whose executor needs the two-connection
// pool guarantee.
func sequenceHasConcurrentBuild(steps []sequenceStep) bool {
	for _, s := range steps {
		if s.kind == StepConcurrentIndexBuild {
			return true
		}
	}
	return false
}

// corroborateValidateCancel disambiguates a statement-cancellation verdict
// on a validate step. SQLSTATE 57014 is query_canceled generally — an
// operator's pg_cancel_backend raises the same code as statement_timeout —
// and the brief mapping reads it as statement-budget exhaustion. That
// conflation is tolerable inside a seconds-scale brief budget, but wrong
// across the validate class's generous budget: a deliberate cancel hours
// early would read as exhaustion, and exhaustion invites escalation to a
// heavier strategy when the cancel means the change should be left alone.
// As in the concurrent build executor, elapsed time corroborates: the
// executor's own statement_timeout cannot fire before the budget elapses,
// so an earlier cancellation came from outside. The original verdict is
// folded into the message, not the chain — the whole point is that this
// failure is not a *BudgetError.
func corroborateValidateCancel(err error, b ValidateBudget, elapsed time.Duration) error {
	var budgetErr *BudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Cause != CauseStatement {
		return err
	}
	if elapsed >= b.Overall {
		return err
	}
	return fmt.Errorf("%w (after %s of a %s budget): %s",
		ErrCancelledExternally, elapsed.Round(time.Millisecond), b.Overall, err.Error())
}

// admitSequence re-parses and classifies every step and verifies each
// targets the preflighted table, before anything executes. A refusal names
// the offending step by 1-based position.
func admitSequence(schema, table string, steps []string) ([]sequenceStep, error) {
	if len(steps) == 0 {
		return nil, ErrEmptySequence
	}
	admitted := make([]sequenceStep, 0, len(steps))
	for i, sql := range steps {
		step, err := admitStep(schema, table, sql)
		if err != nil {
			return nil, fmt.Errorf("sequence step %d of %d: %w", i+1, len(steps), err)
		}
		admitted = append(admitted, step)
	}
	return admitted, nil
}

// admitStep classifies one step by its parsed shape, then verifies its
// target. Only two statement kinds are admissible: a CREATE INDEX
// CONCURRENTLY (delegated to the concurrent build executor) and an ALTER
// TABLE, split into the long-running VALIDATE CONSTRAINT class and the
// brief class everything else runs under. Anything else — including every
// other CONCURRENTLY form — is refused typed. Shape comes before the
// target check so an unsupported statement is reported as the shape
// refusal it is, not as a target mismatch.
func admitStep(schema, table, sql string) (sequenceStep, error) {
	st, err := statement.ParseOne(sql)
	if err != nil {
		return sequenceStep{}, err
	}
	var step sequenceStep
	switch st.Kind() {
	case statement.KindCreateIndex:
		if !st.Concurrent() {
			// A blocking CREATE INDEX never belongs in a safer sequence;
			// the planner emits only the concurrent form.
			return sequenceStep{}, fmt.Errorf("blocking CREATE INDEX: %w", ErrUnsupportedSequenceStep)
		}
		// The delegated executor's statically-decidable admission
		// requirements — a named index, no IF NOT EXISTS, a
		// schema-qualified table — are proven here too: a refusal
		// decidable before anything executes must never fire mid-run
		// after earlier steps committed.
		if _, err := admitConcurrentIndexBuild(sql); err != nil {
			return sequenceStep{}, err
		}
		step = sequenceStep{st: st, kind: StepConcurrentIndexBuild}
	case statement.KindAlterTable:
		if step, err = admitAlterTableStep(st, sql); err != nil {
			return sequenceStep{}, err
		}
	default:
		return sequenceStep{}, fmt.Errorf("statement kind %q: %w", st.Kind(), ErrUnsupportedSequenceStep)
	}
	// INV: ST-7 — every step runs only against the table the preflight
	// proof verified; checked here for the whole sequence so a mixed-target
	// sequence is refused before its first step executes, and re-checked by
	// the brief runner per statement.
	if st.Table() == "" || st.Schema() != schema || st.Table() != table {
		return sequenceStep{}, fmt.Errorf("%w: ST-7: step targets %q but preflight verified %q",
			ErrInvariantViolation, qualifiedName(st.Schema(), st.Table()), qualifiedName(schema, table))
	}
	return step, nil
}

// admitAlterTableStep splits ALTER TABLE steps into their execution
// classes. A VALIDATE CONSTRAINT — and only a lone one — gets the validate
// class: in a multi-operation ALTER the validation shares its transaction
// with the other subcommands, so the whole statement must prove itself
// under the brief budgets instead. Any CONCURRENTLY subcommand (DETACH
// PARTITION CONCURRENTLY) is refused: cancelling its wait leaves a
// detach-pending partition state this executor does not own recovering.
func admitAlterTableStep(st statement.Statement, sql string) (sequenceStep, error) {
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return sequenceStep{}, err
	}
	for _, op := range ops {
		if op.Concurrent {
			return sequenceStep{}, fmt.Errorf("%s: %w", op.Describe(), ErrUnsupportedSequenceStep)
		}
	}
	if len(ops) == 1 && ops[0].Kind == statement.OpValidateConstraint {
		return sequenceStep{st: st, kind: StepValidateConstraint}, nil
	}
	return sequenceStep{st: st, kind: StepBrief}, nil
}
