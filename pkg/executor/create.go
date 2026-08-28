// This file is the create-path executor: it runs a validated desired
// schema — one CREATE TABLE plus its indexes — against a name the caller
// proved absent. Every step is a brief bounded transactional run: the
// table is born this run and carries no traffic, so its indexes are built
// plainly rather than CONCURRENTLY — a plain build on an empty table is
// fast, and unlike CONCURRENTLY it cannot leave an INVALID index behind a
// failure. The executor never trusts the caller's classification (see
// SAFETY.md): each desired statement is qualified into the proof's schema,
// re-parsed by the real grammar, and admitted by shape and target before
// anything executes.
//
// The absence proof is time-of-check: nothing locks the name, so a
// concurrent create can still take it between the check and a step here.
// That loss surfaces as SQLSTATE 42P07 and is returned as the typed
// ErrCreateCollision — the caller re-diffs the live catalog rather than
// assuming what the collision left behind. A failed step ends the run
// immediately; the steps before it committed (each in its own bounded
// transaction) and remain, so a rerun's absence check refuses with
// ErrRelationExists and the declarative front door re-diffs and applies
// the remainder.

package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// Typed refusals and failures for the create path. Admission covers every
// desired statement before the first executes, so a creation this executor
// cannot finish is never started.
var (
	// ErrCreateCollision is returned when a step fails because its target
	// name is already taken. For the table name that means a concurrent
	// create won the race — the absence proof is time-of-check. Index
	// names are never absence-checked, so a pre-existing occupant at an
	// index name reports the same way. Either way the caller re-diffs the
	// live catalog; nothing about the occupant's shape can be assumed.
	ErrCreateCollision = errors.New("a name the create path needs is already taken")
	// ErrDuplicateCreateName is returned when the desired set claims the
	// same relation name twice — two indexes under one name, or an index
	// named after the table. The conflict is decidable before anything
	// runs, so admission refuses the whole set rather than letting a
	// mid-run step fail after a prefix committed.
	ErrDuplicateCreateName = errors.New("desired set claims the same relation name twice")
	// ErrPartitionOfUnsupported is returned for CREATE TABLE ... PARTITION
	// OF: attaching a partition takes a lock on the partitioned parent,
	// an existing table the absence proof says nothing about.
	ErrPartitionOfUnsupported = errors.New("CREATE TABLE PARTITION OF is not supported by the create path: attaching a partition locks the partitioned parent, which the absence proof does not cover")
	// ErrUnsupportedCreateStep is returned when a desired statement is not
	// a shape the create path can run: a plain CREATE TABLE or a plain
	// CREATE INDEX on the new table. CONCURRENTLY is refused deliberately —
	// the table is born this run with no traffic to protect, and a plain
	// build cannot leave an INVALID index behind a failure.
	ErrUnsupportedCreateStep = errors.New("statement is not a shape the create path can run")
)

// The SQLSTATEs a create step raises when its target name is already
// taken. Postgres errors are matched by SQLSTATE, never by message text.
const (
	// sqlstateDuplicateTable: the occupant is a relation — any kind, an
	// index included.
	sqlstateDuplicateTable = "42P07"
	// sqlstateDuplicateObject: the occupant is not a relation — a
	// standalone type under the table's name raises it, because every
	// table also mints a composite type of the same name.
	sqlstateDuplicateObject = "42710"
)

// ExecuteCreate runs the desired schema's statements against the
// verified-absent target: the CREATE TABLE first, then its indexes in
// input order, each step a bounded transactional run under the brief
// budgets, exactly like an optimistic attempt. The pool must come from
// pkg/dbconn. Every desired statement is qualified into the proof's
// schema, re-parsed, and admitted by shape and target before the first
// step executes. On success every step committed and the report says what
// each did. On failure the run stops at the failing step and returns a
// typed *SequenceStepError; the committed prefix remains — a rerun's
// absence check then refuses with preflight.ErrRelationExists, and the
// caller re-diffs the live catalog to apply the remainder. retry bounds
// lock_timeout retries on each step, exactly as in ExecuteNative.
func ExecuteCreate(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, ds statement.DesiredSchema, b Budget, retry RetryPolicy) (SequenceReport, error) {
	return executeCreate(ctx, pool, at, ds, b, retry, nil)
}

// ExecuteCreateWithProgress runs the create path while updating tracker
// with the current step. The caller may poll concurrently.
func ExecuteCreateWithProgress(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, ds statement.DesiredSchema, b Budget, retry RetryPolicy, tracker *progress.Tracker) (rep SequenceReport, err error) {
	if tracker == nil {
		return rep, fmt.Errorf("%w: progress tracker is required", ErrInvariantViolation)
	}
	tracker.Start(len(ds.Statements()), progress.OperationAdmitting)
	defer func() { tracker.Finish(err) }()
	return executeCreate(ctx, pool, at, ds, b, retry, tracker)
}

func executeCreate(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, ds statement.DesiredSchema, b Budget, retry RetryPolicy, tracker *progress.Tracker) (SequenceReport, error) {
	var rep SequenceReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	if err := retry.validate(); err != nil {
		return rep, err
	}
	// INV: ST-7 — the proofs are re-verified at the point of use. Zero
	// values are forgeable by any package: only CheckTableAbsent mints an
	// AbsentTarget with a table, and only ParseDesired mints a
	// DesiredSchema with one.
	if at.Schema() == "" || at.Table() == "" {
		return rep, fmt.Errorf("%w: ST-7: absence proof carries no verified target", ErrInvariantViolation)
	}
	if ds.Table() == "" {
		return rep, fmt.Errorf("%w: ST-7: desired schema carries no admitted CREATE TABLE", ErrInvariantViolation)
	}
	if ds.Table() != at.Table() {
		return rep, fmt.Errorf("%w: ST-7: desired schema targets %q but absence was verified for %q",
			ErrInvariantViolation, ds.Table(), at.Table())
	}
	steps, err := admitCreateSteps(at, ds)
	if err != nil {
		return rep, err
	}
	for i, step := range steps {
		start := time.Now()
		if tracker != nil {
			tracker.StartStep(i+1, progress.OperationBrief)
			start = tracker.Now()
		}
		err := executeWithLockRetryObserved(ctx, retry, func(ctx context.Context) error {
			return executeNativeAttempt(ctx, pool, step, b)
		}, sleepContext, func(attempt int) {
			if tracker != nil {
				tracker.SetAttempt(attempt)
			}
		})
		if err != nil {
			return rep, &SequenceStepError{Step: i + 1, Total: len(steps), Kind: StepBrief, SQL: step.SQL(), Err: asCreateCollision(err)}
		}
		rep.Steps = append(rep.Steps, StepReport{
			SQL:      step.SQL(),
			Kind:     StepBrief,
			Duration: elapsedSince(tracker, start),
		})
	}
	return rep, nil
}

// admitCreateSteps qualifies every desired statement into the proof's
// schema, re-parses it, and admits it by shape and target. The CREATE
// TABLE is ordered first regardless of its input position — an index
// cannot be built before its table exists — and the indexes keep their
// input order after it. Every step claims a name in the same pg_class
// namespace, so a name claimed twice within the set — decidable here —
// is refused before anything runs rather than failing mid-run after a
// prefix committed. A step whose name the server invents (an unnamed
// index) claims nothing decidable and is exempt.
func admitCreateSteps(at preflight.AbsentTarget, ds statement.DesiredSchema) ([]statement.Statement, error) {
	desired := ds.Statements()
	var createStep statement.Statement
	var haveCreate bool
	indexSteps := make([]statement.Statement, 0, len(desired))
	claimed := make(map[string]struct{}, len(desired))
	for i, raw := range desired {
		st, name, err := admitCreateStep(at, raw.SQL())
		if err != nil {
			return nil, fmt.Errorf("desired statement %d of %d: %w", i+1, len(desired), err)
		}
		if name != "" {
			if _, taken := claimed[name]; taken {
				return nil, fmt.Errorf("desired statement %d of %d: %w: %q", i+1, len(desired), ErrDuplicateCreateName, name)
			}
			claimed[name] = struct{}{}
		}
		if st.Kind() == statement.KindCreateTable {
			createStep = st
			haveCreate = true
			continue
		}
		indexSteps = append(indexSteps, st)
	}
	if !haveCreate {
		// A DesiredSchema proof guarantees exactly one CREATE TABLE; a
		// set without one here means the proof was forged or mutated.
		return nil, fmt.Errorf("%w: ST-7: desired schema admitted without a CREATE TABLE", ErrInvariantViolation)
	}
	return append([]statement.Statement{createStep}, indexSteps...), nil
}

// admitCreateStep qualifies one desired statement into the proof's schema,
// re-parses it by the real grammar, and admits it by shape and target. It
// returns the pg_class name the step will claim — the table name, or the
// index name, empty when the server invents one. CREATE TABLE clauses that
// bind to a secondary relation or type — PARTITION OF, INHERITS, LIKE,
// OF — are refused: statement.Qualify rewrites only the target, so the
// secondary name would resolve via search_path to an existing object the
// absence proof says nothing about.
func admitCreateStep(at preflight.AbsentTarget, sql string) (statement.Statement, string, error) {
	qualified, err := statement.Qualify(sql, at.Schema())
	if err != nil {
		return statement.Statement{}, "", err
	}
	st, err := statement.ParseOne(qualified)
	if err != nil {
		return statement.Statement{}, "", err
	}
	ops, err := statement.ParseOps(qualified)
	if err != nil {
		return statement.Statement{}, "", err
	}
	if len(ops) != 1 {
		// ParseOne admitted a single statement, so a differing op count
		// means the two parse boundaries disagree about the same SQL.
		return statement.Statement{}, "", fmt.Errorf("%w: statement carries %d operations", ErrUnsupportedCreateStep, len(ops))
	}
	op := ops[0]
	var claims string
	switch st.Kind() {
	case statement.KindCreateTable:
		if op.PartitionOf {
			return statement.Statement{}, "", ErrPartitionOfUnsupported
		}
		if op.Inherits {
			return statement.Statement{}, "", fmt.Errorf("%w: INHERITS binds to an existing parent the absence proof does not cover", ErrUnsupportedCreateStep)
		}
		if op.Like {
			return statement.Statement{}, "", fmt.Errorf("%w: LIKE reads an existing source table the absence proof does not cover", ErrUnsupportedCreateStep)
		}
		if op.OfType {
			return statement.Statement{}, "", fmt.Errorf("%w: OF binds to an existing composite type the absence proof does not cover", ErrUnsupportedCreateStep)
		}
		if op.IfNotExists {
			return statement.Statement{}, "", ErrIfNotExistsUnsupported
		}
		claims = st.Table()
	case statement.KindCreateIndex:
		if op.Concurrent {
			return statement.Statement{}, "", fmt.Errorf("%w: a concurrent build is refused on a table born this run", ErrUnsupportedCreateStep)
		}
		if op.IfNotExists {
			return statement.Statement{}, "", ErrIfNotExistsUnsupported
		}
		claims = op.Name
	default:
		return statement.Statement{}, "", fmt.Errorf("%w: kind %q", ErrUnsupportedCreateStep, st.Kind())
	}
	// INV: ST-7 — the executor runs exactly the statement that was
	// admitted, and only against the target the absence proof verified.
	if st.Table() == "" || st.Schema() != at.Schema() || st.Table() != at.Table() {
		return statement.Statement{}, "", fmt.Errorf("%w: ST-7: statement targets %q but absence was verified for %q",
			ErrInvariantViolation, qualifiedName(st.Schema(), st.Table()), qualifiedName(at.Schema(), at.Table()))
	}
	return st, claims, nil
}

// asCreateCollision maps the duplicate-name SQLSTATEs — 42P07 when a
// relation holds the name, 42710 when a standalone type does — to the
// typed collision refusal. Every other error passes through unchanged.
// The server's error names the occupant, so the wrap adds the
// classification, not the identifier.
func asCreateCollision(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code != sqlstateDuplicateTable && pgErr.Code != sqlstateDuplicateObject {
		return err
	}
	return fmt.Errorf("%w: %w", ErrCreateCollision, err)
}
