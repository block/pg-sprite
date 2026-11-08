package executor

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// ExecuteCreateWithRowSecurity creates the verified-absent table, its
// indexes, and its complete row security in one transaction. No other
// session can see the table before the transaction commits, so a table whose
// policies are still missing is never visible: an API that exposes new
// tables to clients, or a role with default privileges on the schema, finds
// either no table or the table with its row security in force.
//
// The proofs follow ExecuteCreate: at proves the name was free and cr that
// the session can create in its schema, both minted on this pool just
// before the call. Every table and index statement is qualified into the
// proof's schema, re-parsed, and admitted by shape and target, and the names
// it claims are probed free, before the transaction begins. The row security
// is built in a rolled-back scratch schema first, then rendered from that
// catalog and run with a pg_catalog-only search_path, as in
// ExecuteRowSecurity. Before committing, the transaction reads the new table
// back: it must own every first-choice name it claimed, belong to the
// creation role, and match the desired table and row security exactly. Any
// failure before the commit rolls everything back, including a name an
// occupant took after the probe, which a stepwise create could only report
// after its table committed. A failed commit is a
// *RowSecurityOutcomeUnknownError.
//
// b.StatementTimeout bounds each statement and the whole attempt. No retries
// occur. The caller needs CREATE on the database for the scratch schema.
func ExecuteCreateWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, desired statement.DesiredWithRowSecurity, b Budget) (RowSecurityReport, error) {
	return runCreateWithRowSecurity(ctx, pool, at, cr, desired, b, false)
}

// PreviewCreateWithRowSecurity runs everything ExecuteCreateWithRowSecurity
// runs, including the readback that proves the table and its row security
// converge, and then rolls back. The statements are the ones execution would
// run. Nothing is visible to other sessions at any point.
func PreviewCreateWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, desired statement.DesiredWithRowSecurity, b Budget) (RowSecurityPlan, error) {
	report, err := runCreateWithRowSecurity(ctx, pool, at, cr, desired, b, true)
	if err != nil {
		return RowSecurityPlan{}, err
	}
	return report.plan(), nil
}

func runCreateWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, desired statement.DesiredWithRowSecurity, b Budget, preview bool) (RowSecurityReport, error) {
	if err := b.validate(); err != nil {
		return RowSecurityReport{}, err
	}
	// INV: ST-7 — the proofs are re-verified at the point of use, as in
	// ExecuteCreate.
	if at.Schema() == "" || at.Table() == "" {
		return RowSecurityReport{}, fmt.Errorf("%w: ST-7: absence proof carries no verified target", ErrInvariantViolation)
	}
	if cr.Schema() != at.Schema() {
		return RowSecurityReport{}, fmt.Errorf("%w: ST-7: creation privileges were verified in %q but absence in %q",
			ErrInvariantViolation, cr.Schema(), at.Schema())
	}
	definition := desired.TableDefinition()
	if desired.Table() == "" || definition.Table() == "" {
		return RowSecurityReport{}, fmt.Errorf("%w: ST-7: desired row security carries no admitted CREATE TABLE", ErrInvariantViolation)
	}
	if desired.Table() != at.Table() || definition.Table() != at.Table() {
		return RowSecurityReport{}, fmt.Errorf("%w: ST-7: desired row security targets %q but absence was verified for %q",
			ErrInvariantViolation, desired.Table(), at.Table())
	}
	admitted, err := admitCreateSteps(at, definition)
	if err != nil {
		return RowSecurityReport{}, err
	}
	var claimed []string
	for _, step := range admitted {
		claimed = append(claimed, step.claims...)
	}
	claimed = slices.DeleteFunc(claimed, func(name string) bool { return name == at.Table() })
	// INV: ST-8 — the CREATE TABLE is the first admitted step, so its claims
	// are the names the new table itself must own.
	ownedClaims := slices.DeleteFunc(slices.Clone(admitted[0].claims), func(name string) bool { return name == at.Table() })
	err = preflight.CheckNamesAbsent(ctx, pool, at, claimed)
	if preflight.IsNameOccupied(err) {
		return RowSecurityReport{}, fmt.Errorf("%w: the desired file claims a name the catalog already holds: %w",
			ErrCreateCollision, err)
	}
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("verify claimed names are absent in %s: %w", at.Schema(), err)
	}
	// INV: RS-3 — the whole attempt has one deadline.
	attempt, cancel := context.WithTimeout(ctx, b.StatementTimeout)
	defer cancel()
	report, err := executeCreateWithRowSecurity(attempt, pool, at, cr, desired, admitted, ownedClaims, b, preview)
	return report, rowSecurityError(ctx, attempt, err, b)
}

func executeCreateWithRowSecurity(ctx context.Context, pool *pgxpool.Pool, at preflight.AbsentTarget, cr preflight.CreationRole, desired statement.DesiredWithRowSecurity, admitted []createStep, ownedClaims []string, b Budget, preview bool) (RowSecurityReport, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("begin create with row security: %w", err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	settings := "SET LOCAL lock_timeout = " + strconv.FormatInt(b.LockTimeout.Milliseconds(), 10) + "; SET LOCAL statement_timeout = " + strconv.FormatInt(b.StatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, settings); err != nil {
		return RowSecurityReport{}, fmt.Errorf("set create with row security budgets: %w", err)
	}
	schema, table := at.Schema(), at.Table()
	target := pgx.Identifier{schema, table}.Sanitize()
	if err := checkScratchPrivilege(ctx, tx, target); err != nil {
		return RowSecurityReport{}, err
	}
	// The scratch build runs as the session role and rolls back before the
	// target table exists.
	wanted, err := schemadiff.IntrospectDesiredWithRowSecurityTx(ctx, tx, schema, desired)
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("inspect desired row security for %s: %w", target, classifyRowSecurityDesiredError(err))
	}
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(schema, "public")); err != nil {
		return RowSecurityReport{}, fmt.Errorf("set create search path for %s: %w", target, err)
	}
	if cr.SetsRole() {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{cr.Owner()}.Sanitize()); err != nil {
			return RowSecurityReport{}, fmt.Errorf("set create owner role %s: %w", cr.Owner(), err)
		}
	}
	report := RowSecurityReport{Schema: schema, Table: table, Statements: []string{}}
	for _, step := range admitted {
		if _, err := tx.Exec(ctx, step.statement.SQL()); err != nil {
			return RowSecurityReport{}, fmt.Errorf("create %s: %w", target, asCreateCollision(err))
		}
		report.Statements = append(report.Statements, step.statement.SQL())
	}
	if err := verifyUncommittedNames(ctx, tx, at, ownedClaims); err != nil {
		return RowSecurityReport{}, err
	}
	// INV: ST-9 — a table created under the configured owner must be born
	// under it. The executor never repairs ownership.
	if cr.SetsRole() {
		owner, err := readTableOwner(ctx, tx, at)
		if err != nil {
			return RowSecurityReport{}, fmt.Errorf("read the owner of %s before commit: %w", target, err)
		}
		if owner != cr.Owner() {
			return RowSecurityReport{}, fmt.Errorf("%w: ST-9: %s would be owned by %s, not %s; nothing was committed",
				ErrInvariantViolation, target, owner, cr.Owner())
		}
	}
	security, err := schemadiff.RenderRowSecurity(schema, wanted)
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("render row security for %s: %w: %w", target, ErrRowSecurityRefused, err)
	}
	// INV: RS-4 — helpers are explicitly qualified; no target-schema function
	// may shadow a built-in while replaying policy expressions.
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return RowSecurityReport{}, fmt.Errorf("set row security search path for %s: %w", target, err)
	}
	for _, sql := range security {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return RowSecurityReport{}, fmt.Errorf("apply row security on %s: %w", target, err)
		}
		report.Statements = append(report.Statements, sql)
	}
	// INV: RS-6 — the table and its row security are verified together and
	// commit together.
	actual, err := schemadiff.IntrospectTx(ctx, tx, schema, table)
	if err != nil {
		return RowSecurityReport{}, fmt.Errorf("verify created table %s: %w", target, err)
	}
	if err := admitRowSecurityTable(schema, actual, wanted); err != nil {
		return RowSecurityReport{}, fmt.Errorf("%w: RS-6: created table differs from the desired table: %w", ErrInvariantViolation, err)
	}
	if _, err := schemadiff.DiffWithRowSecurity(schema, actual, wanted); err != nil {
		return RowSecurityReport{}, fmt.Errorf("%w: RS-6: row security did not converge: %w", ErrInvariantViolation, err)
	}
	if preview {
		if err := tx.Rollback(ctx); err != nil {
			return RowSecurityReport{}, fmt.Errorf("roll back the create with row security preview for %s: %w", target, err)
		}
		return report, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return RowSecurityReport{}, &RowSecurityOutcomeUnknownError{Err: err}
	}
	report.TableCreated = true
	return report, nil
}

// verifyUncommittedNames proves the table created in tx owns every
// first-choice name its CREATE TABLE claimed. A missing name means an
// occupant took it after the probe and the server chose another; the caller
// rolls back, so the collision leaves nothing behind.
func verifyUncommittedNames(ctx context.Context, tx pgx.Tx, at preflight.AbsentTarget, claimed []string) error {
	if len(claimed) == 0 {
		return nil
	}
	target := qualifiedName(at.Schema(), at.Table())
	owned, err := preflight.LookupOwnedRelationNames(ctx, tx, at.Schema(), at.Table())
	if err != nil {
		return fmt.Errorf("read the relation names %s owns before commit: %w", target, err)
	}
	missing, unclaimed := ownedNameMismatch(claimed, owned)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s would own %s instead of %s; nothing was committed",
		ErrCreateCollision, target, quotedList(unclaimed), quotedList(missing))
}

// checkScratchPrivilege refuses before any DDL when the session cannot create
// the scratch schema the row security is built in.
func checkScratchPrivilege(ctx context.Context, tx pgx.Tx, target string) error {
	var create bool
	// INV: CO-9 — every function is pg_catalog-qualified.
	err := tx.QueryRow(ctx,
		"SELECT pg_catalog.has_database_privilege(current_user, pg_catalog.current_database(), 'CREATE')").Scan(&create)
	if err != nil {
		return fmt.Errorf("check scratch privilege for %s: %w", target, err)
	}
	if !create {
		return fmt.Errorf("row security on %s requires CREATE on the database for scratch inspection: %w: %w: %w",
			target, ErrRowSecurityRefused, ErrRowSecurityPrivileges, ErrRowSecurityDatabaseCreateRequired)
	}
	return nil
}
