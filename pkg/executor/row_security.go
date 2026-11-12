package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// RowSecurityReport contains only committed RLS statements, in execution order.
// A no-op has an empty Statements slice. Errors return a zero report.
// TableCreated is true when the run created the table along with its row
// security, so Statements begins with its CREATE TABLE. ExtensionSchemas
// is set only then: the schemas the table definition's statements resolved
// unqualified names against, after the schema and public.
type RowSecurityReport struct {
	Schema           string   `json:"schema"`
	Table            string   `json:"table"`
	Statements       []string `json:"statements"`
	TableCreated     bool     `json:"table_created,omitempty"`
	ExtensionSchemas []string `json:"extension_schemas,omitempty"`
}

// plan returns the statements a rolled-back run generated, as a preview.
func (r RowSecurityReport) plan() RowSecurityPlan {
	return RowSecurityPlan{Schema: r.Schema, Table: r.Table, Statements: r.Statements, ExtensionSchemas: r.ExtensionSchemas}
}

// RowSecurityPlan contains generated RLS statements in execution order. A
// preview of an existing table runs no target DDL. A preview of a table that
// does not exist yet runs the whole create transaction on the target,
// readback included, and rolls it back: no catalog change survives, but
// what a rollback does not undo does, such as a sequence an event trigger
// on that DDL advances. A plan is not an execution permission.
type RowSecurityPlan struct {
	Schema     string   `json:"schema"`
	Table      string   `json:"table"`
	Statements []string `json:"statements"`
	// ExtensionSchemas is set for a table that does not exist yet: the
	// schemas its definition's statements resolve unqualified names
	// against, after the schema and public. The row security statements
	// run under pg_catalog alone.
	ExtensionSchemas []string `json:"extension_schemas,omitempty"`
}

// ErrRowSecurityPlanChanged means the locked SQL sequence differs from review.
// No target RLS statements have executed. Preview and review the new plan.
var ErrRowSecurityPlanChanged = errors.New("row security SQL differs from reviewed plan")

type rowSecurityMode uint8

const (
	rowSecurityExecute rowSecurityMode = iota
	rowSecurityPreview
	rowSecurityReviewed
)

// PreviewRowSecurity derives the same SQL as execution under a bounded exclusive
// table lock, then rolls back without changing target policies or settings.
// While held, ACCESS EXCLUSIVE blocks all reads and writes on the target.
// LockTimeout bounds acquisition; StatementTimeout bounds the whole attempt.
func PreviewRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, b Budget) (RowSecurityPlan, error) {
	report, err := runRowSecurity(ctx, pool, schema, desired, b, rowSecurityPreview, nil)
	if err != nil {
		return RowSecurityPlan{}, err
	}
	return report.plan(), nil
}

// ExecuteReviewedRowSecurity regenerates SQL under the target lock and refuses
// any difference from reviewed, including order and duplicates. Empty reviewed
// authorizes only a no-op. The supplied SQL is compared, never executed directly.
// This binds execution to SQL, not to a snapshot of the earlier live definition.
// Budgets, admission, verification and atomicity match ExecuteRowSecurity.
func ExecuteReviewedRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, reviewed []string, b Budget) (RowSecurityReport, error) {
	return runRowSecurity(ctx, pool, schema, desired, b, rowSecurityReviewed, slices.Clone(reviewed))
}

// RowSecurityOutcomeUnknownError means the commit response was lost or failed.
// Inspect the catalog before retrying; an error does not prove rollback here.
type RowSecurityOutcomeUnknownError struct{ Err error }

// Error describes the uncertain commit boundary.
func (e *RowSecurityOutcomeUnknownError) Error() string {
	return fmt.Sprintf("row security commit outcome is unknown; inspect the catalog: %v", e.Err)
}

// Unwrap retains the underlying commit error.
func (e *RowSecurityOutcomeUnknownError) Unwrap() error { return e.Err }

// ExecuteRowSecurity converges only the complete RLS definition of an existing
// ordinary table. It locks and inspects the target itself and refuses table
// deltas. Budget.StatementTimeout also bounds the entire attempt, including
// connection acquisition, scratch inspection, and lock waits. No retries occur.
// The caller must have table-owner and scratch-schema creation privileges.
func ExecuteRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, b Budget) (RowSecurityReport, error) {
	return runRowSecurity(ctx, pool, schema, desired, b, rowSecurityExecute, nil)
}

func runRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, b Budget, mode rowSecurityMode, reviewed []string) (RowSecurityReport, error) {
	if err := b.validate(); err != nil {
		return RowSecurityReport{}, err
	}
	if desired.Table() == "" || schema == "" {
		return RowSecurityReport{}, fmt.Errorf("%w: %w", ErrRowSecurityRefused, statement.ErrRowSecurityDeclaration)
	}
	// INV: RS-3 — the whole attempt has one deadline, not a fresh budget per policy.
	attempt, cancel := context.WithTimeout(ctx, b.StatementTimeout)
	defer cancel()
	report, err := executeRowSecurity(attempt, pool, schema, desired, b, mode, reviewed)
	return report, rowSecurityError(ctx, attempt, err, b)
}

func rowSecurityError(caller, attempt context.Context, err error, b Budget) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*RowSecurityOutcomeUnknownError](err); ok {
		return err
	}
	if caller.Err() != nil && errors.Is(err, caller.Err()) {
		return fmt.Errorf("row security caller stopped: %w", caller.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(attempt.Err(), context.DeadlineExceeded) {
		return &BudgetError{Cause: CauseStatement, Budget: b.StatementTimeout, cause: err}
	}
	if budgetErr := asBudgetError(err, b); budgetErr != nil {
		return budgetErr
	}
	if isRowSecurityAdmissionError(err) {
		return fmt.Errorf("%w: %w", ErrRowSecurityRefused, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return fmt.Errorf("%w: %w: %w", ErrRowSecurityRefused, ErrRowSecurityPrivileges, err)
	}
	return err
}

func executeRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, b Budget, mode rowSecurityMode, reviewed []string) (RowSecurityReport, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("begin row security change: %w", err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	settings := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) + "; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, settings); err != nil {
		return RowSecurityReport{}, fmt.Errorf("set row security budgets: %w", err)
	}
	target := pgx.Identifier{schema, desired.Table()}.Sanitize()
	// Reject missing owner or scratch privileges before taking an application-blocking lock.
	if err := checkRowSecurityPrivileges(ctx, tx, schema, desired.Table()); err != nil {
		return RowSecurityReport{}, err
	}
	// Materialize the declaration before blocking application reads and writes.
	wanted, err := schemadiff.IntrospectDesiredWithRowSecurityTx(ctx, tx, schema, desired)
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("inspect desired row security for %s: %w", target, classifyRowSecurityDesiredError(err))
	}
	// INV: RS-1 — all live comparison and DDL occur after this exclusive lock.
	if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+target+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable {
			return RowSecurityReport{}, fmt.Errorf("lock row security target %s: %w: %w", target, ErrTableNotFound, err)
		}
		return RowSecurityReport{}, fmt.Errorf("lock row security target %s: %w", target, err)
	}
	// Privileges may have changed while waiting for the lock. Recheck them under lock.
	if err := checkRowSecurityPrivileges(ctx, tx, schema, desired.Table()); err != nil {
		return RowSecurityReport{}, err
	}
	live, err := schemadiff.IntrospectTx(ctx, tx, schema, desired.Table())
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("inspect row security target %s: %w", target, err)
	}
	if err := admitRowSecurityTable(schema, live, wanted); err != nil {
		return RowSecurityReport{}, fmt.Errorf("admit row security target %s: %w: %w", target, ErrRowSecurityRefused, err)
	}
	report := RowSecurityReport{Schema: schema, Table: desired.Table(), Statements: []string{}}
	if _, err := schemadiff.DiffWithRowSecurity(schema, live, wanted); err != nil {
		for _, policy := range live.RowSecurity.Policies {
			report.Statements = append(report.Statements, "DROP POLICY "+pgx.Identifier{policy.Name}.Sanitize()+" ON "+target)
		}
		security, err := schemadiff.RenderRowSecurity(schema, wanted)
		if err != nil {
			return RowSecurityReport{}, fmt.Errorf("render row security for %s: %w: %w", target, ErrRowSecurityRefused, err)
		}
		report.Statements = append(report.Statements, security...)

	}
	// Checked under the target lock and before the review comparison, so a
	// preview refuses the same policies an execution would. Creating a table
	// needs no such check: no existing policy or view can read a table that
	// does not exist yet.
	if len(report.Statements) > 0 {
		if err := refusePolicyRecursion(ctx, tx, schema, desired); err != nil {
			return RowSecurityReport{}, err
		}
	}
	// INV: RS-5 — compare the complete ordered sequence under lock before target DDL.
	if mode == rowSecurityReviewed && !slices.Equal(reviewed, report.Statements) {
		return RowSecurityReport{}, fmt.Errorf("review row security on %s: %w", target, ErrRowSecurityPlanChanged)
	}
	if mode == rowSecurityPreview {
		if err := tx.Rollback(ctx); err != nil {
			return RowSecurityReport{}, fmt.Errorf("rollback row security preview for %s: %w", target, err)
		}
		return report, nil
	}
	// INV: RS-4 — helpers are explicitly qualified; no target-schema function
	// may shadow a built-in while replaying policy expressions.
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return RowSecurityReport{}, fmt.Errorf("set row security search path for %s: %w", target, err)
	}
	for _, sql := range report.Statements {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return RowSecurityReport{}, fmt.Errorf("apply row security on %s: %w", target, err)
		}
	}

	// INV: RS-2 — verify convergence before committing, while retaining the lock.
	actual, err := schemadiff.IntrospectTx(ctx, tx, schema, desired.Table())
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("verify row security target %s: %w", target, err)
	}
	if err := admitRowSecurityTable(schema, actual, wanted); err != nil {
		return RowSecurityReport{}, fmt.Errorf("%w: RS-2: target shape changed: %w", ErrInvariantViolation, err)
	}
	if _, err := schemadiff.DiffWithRowSecurity(schema, actual, wanted); err != nil {
		return RowSecurityReport{}, fmt.Errorf("%w: RS-2: row security did not converge: %w", ErrInvariantViolation, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RowSecurityReport{}, &RowSecurityOutcomeUnknownError{Err: err}
	}
	return report, nil
}

func admitRowSecurityTable(schema string, live, wanted schemadiff.Model) error {
	table, err := rowSecurityTableShape(live)
	if err != nil {
		return err
	}
	definition := wanted
	definition.RowSecurity = schemadiff.RowSecurity{}
	changes, err := schemadiff.Diff(schema, table, definition)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	if _, err := schemadiff.DiffWithRowSecurity(schema, live, wanted); errors.Is(err, schemadiff.ErrOnlyTableDiffers) {
		return fmt.Errorf("this executor changes only row security; run the table change with migrate.RunDesiredWithRowSecurity, which leaves the row security as it is: %w", err)
	}
	return fmt.Errorf("mixed table and row security changes: %w", schemadiff.ErrUnsupportedChange)
}

// rowSecurityTableShape returns m without its row security or the facts
// only an export refuses, and refuses a table shape the row security path
// does not support. Render refuses unsupported shapes even when two models
// happen to match.
func rowSecurityTableShape(m schemadiff.Model) (schemadiff.Model, error) {
	table := withoutExportOnlyRefusals(m)
	table.RowSecurity = schemadiff.RowSecurity{}
	if _, err := schemadiff.Render(table); err != nil {
		return schemadiff.Model{}, fmt.Errorf("row security target: %w", err)
	}
	return table, nil
}

// withoutExportOnlyRefusals returns live without the facts that only an
// export of the table refuses. A policy change builds nothing from the
// table's rendering, so neither hazard those facts guard applies: a
// rebuilt table might have no TOAST table to hold its toast.* parameters,
// and the serial shorthand cannot state a sequence declared away from its
// defaults. A serial's declaration is not compared by the diff either, so
// clearing it changes nothing else.
func withoutExportOnlyRefusals(live schemadiff.Model) schemadiff.Model {
	table := live
	table.ToastMayNotRebuild = false
	table.Columns = slices.Clone(live.Columns)
	for i, c := range table.Columns {
		if c.SequenceDefault && c.Identity == schemadiff.IdentityNone {
			table.Columns[i].Sequence = nil
		}
	}
	return table
}

// isRowSecurityAdmissionError reports whether err is a desired file's
// row-security declaration refused at admission: a property of the file, so
// retrying cannot change it.
func isRowSecurityAdmissionError(err error) bool {
	for _, cause := range []error{
		statement.ErrPolicyRelationDependency,
		statement.ErrPolicyReadsOwnTable,
		statement.ErrPolicyReadsCatalog,
		statement.ErrPolicyLocksRows,
		statement.ErrRowSecurityDeclaration,
	} {
		if errors.Is(err, cause) {
			return true
		}
	}
	return false
}
