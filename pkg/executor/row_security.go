package executor

import (
	"context"
	"errors"
	"fmt"
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
type RowSecurityReport struct {
	Schema     string   `json:"schema"`
	Table      string   `json:"table"`
	Statements []string `json:"statements"`
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
	if err := b.validate(); err != nil {
		return RowSecurityReport{}, err
	}
	if desired.Table() == "" || schema == "" {
		return RowSecurityReport{}, fmt.Errorf("%w: %w", ErrRowSecurityRefused, statement.ErrRowSecurityDeclaration)
	}
	// INV: RS-3 — the whole attempt has one deadline, not a fresh budget per policy.
	attempt, cancel := context.WithTimeout(ctx, b.StatementTimeout)
	defer cancel()
	report, err := executeRowSecurity(attempt, pool, schema, desired, b)
	return report, rowSecurityError(ctx, attempt, err, b)
}

func rowSecurityError(caller, attempt context.Context, err error, b Budget) error {
	if err == nil {
		return nil
	}
	var unknown *RowSecurityOutcomeUnknownError
	if errors.As(err, &unknown) {
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
	if errors.Is(err, statement.ErrPolicyRelationDependency) || errors.Is(err, statement.ErrRowSecurityDeclaration) {
		return fmt.Errorf("%w: %w", ErrRowSecurityRefused, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return fmt.Errorf("%w: %w", ErrRowSecurityRefused, err)
	}
	return err
}

func executeRowSecurity(ctx context.Context, pool *pgxpool.Pool, schema string, desired statement.DesiredWithRowSecurity, b Budget) (RowSecurityReport, error) {
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
	wanted, err := schemadiff.IntrospectDesiredWithRowSecurityTx(ctx, tx, desired)
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("inspect desired row security for %s: %w", target, classifyRowSecurityDesiredError(err))
	}
	if err := admitRowSecurityTable(schema, live, wanted); err != nil {
		return RowSecurityReport{}, fmt.Errorf("admit row security target %s: %w: %w", target, ErrRowSecurityRefused, err)
	}
	report := RowSecurityReport{Schema: schema, Table: desired.Table(), Statements: []string{}}
	if _, err := schemadiff.DiffWithRowSecurity(schema, live, wanted); err != nil {
		// INV: RS-4 — helpers are explicitly qualified; no target-schema function
		// may shadow a built-in while replaying policy expressions.
		if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
			return RowSecurityReport{}, fmt.Errorf("set row security search path for %s: %w", target, err)
		}
		for _, policy := range live.RowSecurity.Policies {
			report.Statements = append(report.Statements, "DROP POLICY "+pgx.Identifier{policy.Name}.Sanitize()+" ON "+target)
		}
		security, err := schemadiff.RenderRowSecurity(schema, wanted)
		if err != nil {
			return RowSecurityReport{}, fmt.Errorf("render row security for %s: %w: %w", target, ErrRowSecurityRefused, err)
		}
		report.Statements = append(report.Statements, security...)
		for _, sql := range report.Statements {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return RowSecurityReport{}, fmt.Errorf("apply row security on %s: %w", target, err)
			}
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
	// Render refuses unsupported shapes even when two models happen to match.
	table := live
	table.RowSecurity = schemadiff.RowSecurity{}
	if _, err := schemadiff.Render(table); err != nil {
		return fmt.Errorf("row security target: %w", err)
	}
	wanted.RowSecurity = schemadiff.RowSecurity{}
	changes, err := schemadiff.Diff(schema, table, wanted)
	if err != nil {
		return err
	}
	if len(changes) != 0 {
		return fmt.Errorf("mixed table and row security changes: %w", schemadiff.ErrUnsupportedChange)
	}
	return nil
}
