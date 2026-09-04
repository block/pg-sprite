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
// Before the first step the executor probes pg_class for every name the
// desired file states — explicit index names and the first-choice names of
// index-backed constraints and column-owned sequences — so an occupied
// name refuses the whole set rather than failing after the table committed;
// the table name itself is the caller's absence proof. The proof is
// time-of-check: nothing locks the names, so a concurrent create can still
// take one before its step. Duplicate-name SQLSTATEs backstop races for
// explicit names. For server-chosen names, the probe narrows the race to
// the time-of-check window, but nothing catches a name taken inside it. A
// failed step ends the run immediately; the steps before it committed
// (each in its own bounded transaction) remain, so a rerun's absence check refuses with
// ErrRelationExists and the declarative front door re-diffs and applies
// the remainder.

package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// ErrCreateCollision is returned when a claimed name is already taken.
	// A catalog probe after admission checks the desired file's explicit
	// index names and first-choice constraint-index and sequence names before
	// the first step executes, refusing the whole set with no *SequenceStepError;
	// duplicate-name SQLSTATEs backstop time-of-check races for explicit
	// names and arrive wrapped in one. Server-chosen names have no such
	// backstop. The remedy is on the desired file's side — drop or rename the
	// occupant, name a constraint's index explicitly, or for a sequence use
	// an explicitly named sequence or a non-serial column —
	// then re-diff; a re-plan alone reproduces the refusal.
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
// budgets, exactly like an optimistic attempt, with its search_path
// pinned to the proof's schema then public — the same policy the
// introspection read path sets — so the desired file's unqualified
// references resolve exactly as the diff resolved them. The pool must
// come from pkg/dbconn and must be the session the proofs were minted on:
// cr proves that session's role can create in the proof's schema, and
// like the absence proof it is time-of-check — a grant revoked after
// minting fails with the server's own error. Every desired statement is
// qualified into the proof's schema, re-parsed, and admitted by shape and
// target, and every index and constraint-index name the file states is
// probed free in pg_class, before the first step executes. A refusal from
// either check returns with an empty report and no *SequenceStepError:
// an occupied name is ErrCreateCollision, an inadmissible shape one of the
// admission sentinels, and a probe that could not complete (a cancelled
// context, a lost connection) is that error, wrapped — not a collision.
// On success every step committed and the report says what each did. On
// failure the run stops at the failing step and returns a typed
// *SequenceStepError; the committed prefix remains — a rerun's absence
// check then refuses with preflight.ErrRelationExists, and the caller
// re-diffs the live catalog to apply the remainder. retry bounds
// lock_timeout retries on each step, exactly as in ExecuteNative.
func ExecuteCreate(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, ds statement.DesiredSchema, b Budget, retry RetryPolicy) (SequenceReport, error) {
	return executeCreate(ctx, pool, at, cr, ds, b, retry, nil)
}

// ExecuteCreateWithProgress runs the create path while updating tracker
// with the current step. The caller may poll concurrently.
func ExecuteCreateWithProgress(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, ds statement.DesiredSchema, b Budget, retry RetryPolicy, tracker *progress.Tracker) (rep SequenceReport, err error) {
	if tracker == nil {
		return rep, fmt.Errorf("%w: progress tracker is required", ErrInvariantViolation)
	}
	tracker.Start(len(ds.Statements()), progress.OperationAdmitting)
	defer func() { tracker.Finish(err) }()
	return executeCreate(ctx, pool, at, cr, ds, b, retry, tracker)
}

func executeCreate(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, ds statement.DesiredSchema, b Budget, retry RetryPolicy, tracker *progress.Tracker) (SequenceReport, error) {
	var rep SequenceReport
	if err := b.validate(); err != nil {
		return rep, err
	}
	if err := retry.validate(); err != nil {
		return rep, err
	}
	// INV: ST-7 — the proofs are re-verified at the point of use. Zero
	// values are forgeable by any package: only CheckTableAbsent mints an
	// AbsentTarget with a table, only CheckCreatePrivileges mints a
	// CreationRole with a schema, and only ParseDesired mints a
	// DesiredSchema with a table.
	if at.Schema() == "" || at.Table() == "" {
		return rep, fmt.Errorf("%w: ST-7: absence proof carries no verified target", ErrInvariantViolation)
	}
	if cr.Schema() == "" {
		return rep, fmt.Errorf("%w: ST-7: creation-privilege proof carries no verified schema", ErrInvariantViolation)
	}
	if cr.Schema() != at.Schema() {
		return rep, fmt.Errorf("%w: ST-7: creation privileges were verified in %q but absence in %q",
			ErrInvariantViolation, cr.Schema(), at.Schema())
	}
	if ds.Table() == "" {
		return rep, fmt.Errorf("%w: ST-7: desired schema carries no admitted CREATE TABLE", ErrInvariantViolation)
	}
	if ds.Table() != at.Table() {
		return rep, fmt.Errorf("%w: ST-7: desired schema targets %q but absence was verified for %q",
			ErrInvariantViolation, ds.Table(), at.Table())
	}
	steps, claimed, err := admitCreateSteps(at, ds)
	if err != nil {
		return rep, err
	}
	// Every deterministic relation name the desired set will claim is
	// proved free before the first step executes, so an occupied index name
	// refuses the whole set instead of failing after the table committed.
	// CheckTableAbsent already covers the table name and its composite type.
	claimed = slices.DeleteFunc(claimed, func(name string) bool { return name == at.Table() })
	err = preflight.CheckNamesAbsent(ctx, pool, at, claimed)
	if preflight.IsNameOccupied(err) {
		return rep, fmt.Errorf("%w: the desired file claims a name the catalog already holds: %w",
			ErrCreateCollision, err)
	}
	if err != nil {
		// The probe itself failed — a cancelled context, a dropped
		// connection — which says nothing about whether the names are
		// free; it is an operational failure, not a collision.
		return rep, fmt.Errorf("verify claimed names are absent in %s: %w", at.Schema(), err)
	}
	for i, step := range steps {
		start := time.Now()
		if tracker != nil {
			tracker.StartStep(i+1, progress.OperationBrief, step.SQL())
			start = tracker.Now()
		}
		err := executeWithLockRetryObserved(ctx, retry, func(ctx context.Context) error {
			return executeBoundedAttempt(ctx, pool, step, b, at.Schema())
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
// schema, re-parses it, and admits it by shape and target. The statements
// arrive in execution order — the CREATE TABLE first, the indexes in input
// order after it, ordered once by statement.ParseDesired — and the steps
// keep that order. Every step claims the names it will occupy in the
// same pg_class namespace — the table plus the first-choice relation names
// of its constraints and column-owned sequences, or an explicit index name.
// A name claimed twice within the set — decidable here — is refused before
// anything runs rather than failing mid-run after a prefix committed.
// The claims are first choices: a set whose first choices collide is
// refused even where the server would sidestep with a numeric suffix,
// because a deterministic name the file states beats one the server
// invents. A step whose name the server invents outright (an unnamed
// index) claims nothing decidable and is exempt.
// The admitted steps are returned with every name they claim, so the caller
// can prove the set free in the catalog before the first step executes;
// duplicates within the set are already refused here, so the names are
// distinct. Order does not matter: the catalog probe is set membership and
// picks the reported occupant itself.
func admitCreateSteps(at preflight.AbsentTarget, ds statement.DesiredSchema) ([]statement.Statement, []string, error) {
	checked, err := checkCreateSteps(at.Schema(), ds)
	if err != nil {
		return nil, nil, err
	}
	steps := make([]statement.Statement, 0, len(checked))
	names := make([]string, 0, len(checked))
	for i, step := range checked {
		if step.refusal != nil {
			return nil, nil, fmt.Errorf("desired statement %d of %d: %w", i+1, len(checked), step.refusal)
		}
		steps = append(steps, step.statement)
		names = append(names, step.claims...)
	}
	return steps, names, nil
}

// CreateShapeRefusals checks the connection-free create-path rules in desired
// statement order. A nil entry is admitted; a non-nil entry identifies the
// shape refusal for that statement. Parse failures and violated DesiredSchema
// invariants are returned separately because no positional plan is safe.
func CreateShapeRefusals(schema string, ds statement.DesiredSchema) ([]error, error) {
	checked, err := checkCreateSteps(schema, ds)
	if err != nil {
		return nil, err
	}
	refusals := make([]error, len(checked))
	for i, step := range checked {
		refusals[i] = step.refusal
	}
	return refusals, nil
}

// createStep is one desired statement after shape checking: the qualified,
// re-parsed statement, the pg_class names it will claim, and the shape
// refusal that keeps it from running, nil when admitted.
type createStep struct {
	statement statement.Statement
	claims    []string
	refusal   error
}

// checkCreateSteps shape-checks every desired statement in order and marks
// the second claimant of any name with ErrDuplicateCreateName. A step's
// claims register whether or not its shape is refused, so a later statement
// that collides with a refused one is reported as the collision it is rather
// than admitted; a shape refusal already on the step is kept as its cause.
// A returned error means no positional result is safe — a parse failure or
// a violated DesiredSchema invariant.
func checkCreateSteps(schema string, ds statement.DesiredSchema) ([]createStep, error) {
	desired := ds.Statements()
	// INV: ST-8 — a DesiredSchema proof guarantees a CREATE TABLE ordered
	// first; a set that does not lead with one means the proof was forged
	// or mutated.
	if len(desired) == 0 || desired[0].Kind() != statement.KindCreateTable {
		return nil, fmt.Errorf("%w: ST-8: desired schema does not lead with a CREATE TABLE", ErrInvariantViolation)
	}
	steps := make([]createStep, 0, len(desired))
	claimed := make(map[string]struct{}, len(desired))
	for i, raw := range desired {
		step, err := checkCreateStepShape(schema, ds.Table(), raw.SQL())
		if err != nil {
			return nil, fmt.Errorf("desired statement %d of %d: %w", i+1, len(desired), err)
		}
		for _, name := range step.claims {
			if _, taken := claimed[name]; taken {
				if step.refusal == nil {
					step.refusal = fmt.Errorf("%w: %q", ErrDuplicateCreateName, name)
				}
				continue
			}
			claimed[name] = struct{}{}
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// checkCreateStepShape qualifies one desired statement into the target schema,
// re-parses it by the real grammar, and checks it by shape and target. It
// returns the pg_class names the step will claim — for a CREATE TABLE the
// table name plus the first-choice relation names of its constraints and
// column-owned sequences, for a CREATE INDEX its explicit name, nothing when the
// server invents one. CREATE TABLE clauses that bind to a secondary
// relation or type — PARTITION OF, INHERITS, LIKE, OF — are refused:
// statement.Qualify rewrites only the target, so the secondary name would
// resolve via search_path to an existing object the absence proof says
// nothing about.
func checkCreateStepShape(schema, table, sql string) (createStep, error) {
	qualified, err := statement.Qualify(sql, schema)
	if err != nil {
		return createStep{}, err
	}
	st, err := statement.ParseOne(qualified)
	if err != nil {
		return createStep{}, err
	}
	ops, err := statement.ParseOps(qualified)
	if err != nil {
		return createStep{}, err
	}
	// INV: ST-7 — the executor runs exactly the statement that was
	// admitted, and only against the desired schema's own table; on the
	// execution path admitCreateSteps hands in the schema the absence proof
	// verified, and executeCreate has already matched the proof's table to
	// the desired schema's.
	if st.Table() == "" || st.Schema() != schema || st.Table() != table {
		return createStep{}, fmt.Errorf("%w: ST-7: statement targets %q but desired schema is for %q",
			ErrInvariantViolation, qualifiedName(st.Schema(), st.Table()), qualifiedName(schema, table))
	}
	step := createStep{statement: st}
	if len(ops) != 1 {
		// ParseOne admitted a single statement, so a differing op count
		// means the two parse boundaries disagree about the same SQL.
		step.refusal = fmt.Errorf("%w: statement carries %d operations", ErrUnsupportedCreateStep, len(ops))
		return step, nil
	}
	op := ops[0]
	switch st.Kind() {
	case statement.KindCreateTable:
		step.claims, step.refusal = checkCreateTableShape(qualified, st.Table(), op)
	case statement.KindCreateIndex:
		step.claims, step.refusal = checkCreateIndexShape(op)
	default:
		step.refusal = fmt.Errorf("%w: kind %q", ErrUnsupportedCreateStep, st.Kind())
	}
	return step, nil
}

// checkCreateTableShape refuses the CREATE TABLE clauses that bind to a
// secondary relation or type and returns the names the table will claim:
// its own plus the first-choice relation names of its constraints and
// column-owned sequences.
// The claims are returned with the refusal so a later statement colliding
// with a refused table is still reported.
func checkCreateTableShape(qualified, table string, op statement.Op) ([]string, error) {
	implicit, err := statement.ImplicitRelationNames(qualified)
	if err != nil {
		// ParseOne already admitted this SQL as a CREATE TABLE, so a
		// refusal here means the two parse boundaries disagree.
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedCreateStep, err)
	}
	return append([]string{table}, implicit...), createTableShapeRefusal(op)
}

// createTableShapeRefusal names the CREATE TABLE clause that keeps the
// statement off the create path, nil when the shape is admitted.
func createTableShapeRefusal(op statement.Op) error {
	if op.PartitionOf {
		return ErrPartitionOfUnsupported
	}
	if op.Inherits {
		return fmt.Errorf("%w: INHERITS binds to an existing parent the absence proof does not cover", ErrUnsupportedCreateStep)
	}
	if op.Like {
		return fmt.Errorf("%w: LIKE reads an existing source table the absence proof does not cover", ErrUnsupportedCreateStep)
	}
	if op.OfType {
		return fmt.Errorf("%w: OF binds to an existing composite type the absence proof does not cover", ErrUnsupportedCreateStep)
	}
	if op.IfNotExists {
		return ErrIfNotExistsUnsupported
	}
	return nil
}

// checkCreateIndexShape refuses index builds that cannot run against a
// table born this run and returns the explicit index name as the step's
// claim; an unnamed index claims nothing decidable. The claim is returned
// with the refusal so a later statement colliding with a refused index is
// still reported.
func checkCreateIndexShape(op statement.Op) ([]string, error) {
	var claims []string
	if op.Name != "" {
		claims = []string{op.Name}
	}
	return claims, createIndexShapeRefusal(op)
}

// createIndexShapeRefusal names the CREATE INDEX clause that keeps the
// statement off the create path, nil when the shape is admitted.
func createIndexShapeRefusal(op statement.Op) error {
	if op.Concurrent {
		return fmt.Errorf("%w: a concurrent build is refused on a table born this run", ErrUnsupportedCreateStep)
	}
	if op.IfNotExists {
		return ErrIfNotExistsUnsupported
	}
	return nil
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
