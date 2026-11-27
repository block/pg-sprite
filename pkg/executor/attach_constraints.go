package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
)

// The constraint step of an online ATTACH PARTITION. The attach refuses a
// table that lacks a CHECK constraint of the parent, under the parent's
// name and validated, or that allows NULL in a column the parent declares
// NOT NULL. Neither is fixed online by the plain statements: ADD CONSTRAINT
// ... CHECK and SET NOT NULL each scan the table under ACCESS EXCLUSIVE.
// This file gives the table both first, while writes go on:
//
//   - each CHECK of the parent is added under its name NOT VALID, then
//     validated under SHARE UPDATE EXCLUSIVE (ensureAttachProof);
//   - each column the parent declares NOT NULL gets a helper CHECK (column
//     IS NOT NULL), added NOT VALID and validated the same way, then SET NOT
//     NULL in a brief transaction, which the validated CHECK lets skip its
//     scan, and the helper is dropped.
//
// Every step reads the catalog and decides, so a rerun resumes, and a helper
// left behind by a stop after SET NOT NULL is dropped by the next call.

var (
	// errAttachColumnMissing is returned when the table lacks a column the
	// parent declares NOT NULL, which the attach would refuse anyway.
	errAttachColumnMissing = errors.New("the table lacks a column of the partitioned parent")
	// errAttachNotNullNotValid is returned for a column whose not-null
	// constraint is NOT VALID (PostgreSQL 18). Setting it NOT NULL would
	// validate it under ACCESS EXCLUSIVE.
	errAttachNotNullNotValid = errors.New("the column's not-null constraint is NOT VALID")
)

// attachConstraintAction says what the step did for one of the parent's
// constraints.
type attachConstraintAction string

const (
	// attachConstraintFound: the table already carried it.
	attachConstraintFound attachConstraintAction = "found"
	// attachConstraintValidated: a CHECK under the parent's name was NOT
	// VALID, and the step validated it.
	attachConstraintValidated attachConstraintAction = "validated"
	// attachConstraintAdded: the step added the CHECK and validated it.
	attachConstraintAdded attachConstraintAction = "added"
	// attachConstraintSetNotNull: the step proved the column holds no NULL
	// and set it NOT NULL.
	attachConstraintSetNotNull attachConstraintAction = "set-not-null"
)

// attachConstraintResult is what the step did for one CHECK or NOT NULL
// column of the parent.
type attachConstraintResult struct {
	// check is the CHECK's name, or empty for a NOT NULL column.
	check string
	// column is the NOT NULL column, or empty for a CHECK.
	column string
	action attachConstraintAction
}

// attachTable is the table an attach will attach, as resolved when the run
// began.
type attachTable struct {
	oid    uint32
	schema string
	name   string
}

func (t attachTable) qualified() string { return qualifiedName(t.schema, t.name) }

// proof is an attach proof of kind on the table.
func (t attachTable) proof(kind attachProofKind, expr, constraint string) attachProof {
	return attachProof{kind: kind, tableOID: t.oid, schema: t.schema, table: t.name, expr: expr, constraint: constraint}
}

// ensureAttachConstraints gives the table every CHECK constraint of the
// parent, then every NOT NULL the parent declares, and returns what it did
// for each, CHECKs first. Each validation is bounded by b. It stops at the
// first error and keeps what it finished. The caller holds the parent's
// advisory table lock (LK-1).
func ensureAttachConstraints(ctx context.Context, pool *pgxpool.Pool, parentOID uint32, table attachTable, b ValidateBudget,
	holderWait time.Duration, tracker *progress.Tracker, started attachProofStatementStarted) ([]attachConstraintResult, error) {
	tx := briefTx{pool: pool}
	var (
		checks   []parentCheck
		notNulls []string
	)
	err := tx.read(ctx, "read the constraints of the partitioned parent of "+table.qualified(), func(ctx context.Context, q querier) error {
		var err error
		if checks, err = readParentChecks(ctx, q, parentOID); err != nil {
			return err
		}
		notNulls, err = readParentNotNullColumns(ctx, q, parentOID)
		return err
	})
	if err != nil {
		return nil, err
	}
	results := make([]attachConstraintResult, 0, len(checks)+len(notNulls))
	for _, c := range checks {
		res, err := ensureAttachProof(ctx, pool, table.proof(attachProofParent, c.expr, c.name), b, holderWait, tracker, started)
		if err != nil {
			return results, fmt.Errorf("give %s the parent's CHECK %s: %w", table.qualified(), c.name, err)
		}
		results = append(results, attachConstraintResult{check: c.name, action: checkAction(res.action)})
	}
	for _, column := range notNulls {
		action, err := ensureAttachNotNull(ctx, pool, tx, table, column, b, holderWait, tracker, started)
		if err != nil {
			return results, fmt.Errorf("make column %q of %s NOT NULL: %w", column, table.qualified(), err)
		}
		results = append(results, attachConstraintResult{column: column, action: action})
	}
	return results, nil
}

// checkAction is the proof step's action as the constraint step reports it.
func checkAction(a attachProofAction) attachConstraintAction {
	switch a {
	case attachProofAdded:
		return attachConstraintAdded
	case attachProofValidated:
		return attachConstraintValidated
	default:
		return attachConstraintFound
	}
}

// ensureAttachNotNull brings one column to NOT NULL online: the helper CHECK
// proves it holds no NULL, SET NOT NULL then skips its scan, and the helper
// is dropped. A column already NOT NULL only has a leftover helper dropped.
func ensureAttachNotNull(ctx context.Context, pool *pgxpool.Pool, tx briefTx, table attachTable, column string, b ValidateBudget,
	holderWait time.Duration, tracker *progress.Tracker, started attachProofStatementStarted) (attachConstraintAction, error) {
	helper := table.proof(attachProofNotNull, pgx.Identifier{column}.Sanitize()+" IS NOT NULL", "")
	notNull, err := readAttachColumnNotNull(ctx, tx, table, column)
	if err != nil {
		return "", err
	}
	action := attachConstraintFound
	if !notNull {
		if _, err := ensureAttachProof(ctx, pool, helper, b, holderWait, tracker, started); err != nil {
			return "", err
		}
		setSQL := "ALTER TABLE " + pgx.Identifier{table.schema, table.name}.Sanitize() +
			" ALTER COLUMN " + pgx.Identifier{column}.Sanitize() + " SET NOT NULL"
		// INV: LK-2 — SET NOT NULL takes ACCESS EXCLUSIVE, so it requests its
		// lock once no long-lived reader or writer holds the table, or once
		// holderWait runs out.
		if _, err := waitOutsideLockQueue(ctx, pool, helper.lockedTables(), holderWait, systemHolderWaitClock(), trackerHolderWaitReport(tracker)); err != nil {
			return "", err
		}
		reportAttachProofStatement(started, progress.OperationBrief, setSQL)
		if err := setAttachNotNull(ctx, tx, helper, column, setSQL); err != nil {
			return "", err
		}
		action = attachConstraintSetNotNull
	}
	if err := dropAttachHelper(ctx, pool, tx, helper, holderWait, tracker, started); err != nil {
		return "", err
	}
	return action, nil
}

// readAttachColumnNotNull reads whether the column refuses NULL. A column
// the table lacks, and a NOT VALID not-null constraint, are refused.
func readAttachColumnNotNull(ctx context.Context, tx briefTx, table attachTable, column string) (bool, error) {
	var found, notNull, notValid bool
	err := tx.read(ctx, "read column "+column+" of "+table.qualified(), func(ctx context.Context, q querier) error {
		err := q.QueryRow(ctx, attachColumnSQL, table.oid, column).Scan(&notNull, &notValid)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("%w: %s has no column %q", errAttachColumnMissing, table.qualified(), column)
	}
	if notValid {
		return false, fmt.Errorf("%w: column %q of %s", errAttachNotNullNotValid, column, table.qualified())
	}
	return notNull, nil
}

// setAttachNotNull runs SET NOT NULL in one brief transaction. Under the
// table's lock it proves the table and that the helper CHECK is still there
// and validated, so the statement skips its scan; it reads back the column
// NOT NULL before the commit.
func setAttachNotNull(ctx context.Context, tx briefTx, helper attachProof, column, setSQL string) error {
	return tx.run(ctx, "set column "+column+" of "+helper.qualified()+" NOT NULL", catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// INV: ST-7 — the table, and its constraints, stay as proven
			// until the transaction ends.
			if err := lockAndProveAttachProofTable(ctx, tx, helper); err != nil {
				return err
			}
			state, _, err := readAttachProof(ctx, tx, helper)
			if err != nil {
				return err
			}
			if state != attachProofDone {
				return fmt.Errorf("%w: ST-7: the helper CHECK %s on %s is in state %d before SET NOT NULL, want validated",
					ErrInvariantViolation, helper.name(), helper.qualified(), state)
			}
			// INV: LK-2 — the validated helper CHECK implies the column holds
			// no NULL, so the statement scans nothing, under the pool's
			// lock_timeout and statement_timeout.
			if _, err := tx.Exec(ctx, setSQL); err != nil {
				return err
			}
			var notNull bool
			if err := tx.QueryRow(ctx, attachColumnSQL, helper.tableOID, column).Scan(&notNull, new(bool)); err != nil {
				return fmt.Errorf("read back column %q of %s: %w", column, helper.qualified(), err)
			}
			if !notNull {
				return fmt.Errorf("%w: column %q of %s allows NULL after SET NOT NULL", ErrInvariantViolation, column, helper.qualified())
			}
			return nil
		})
}

// dropAttachHelper drops the helper CHECK when one is there
// (dropAttachProof), reporting the statement only when it runs.
func dropAttachHelper(ctx context.Context, pool *pgxpool.Pool, tx briefTx, helper attachProof,
	holderWait time.Duration, tracker *progress.Tracker, started attachProofStatementStarted) error {
	var present bool
	if err := tx.read(ctx, "inspect the helper CHECK of "+helper.qualified(), func(ctx context.Context, q querier) error {
		state, _, err := readAttachProof(ctx, q, helper)
		present = state != attachProofAddNeeded
		return err
	}); err != nil || !present {
		return err
	}
	reportAttachProofStatement(started, progress.OperationBrief, helper.dropSQL())
	return dropAttachProof(ctx, pool, helper, holderWait, tracker)
}

// attachColumnSQL reads whether a column refuses NULL, and whether its
// not-null constraint is NOT VALID (only PostgreSQL 18 catalogs one). Every
// relation and operator is pg_catalog-qualified (CO-9).
const attachColumnSQL = `SELECT a.attnotnull,
	       EXISTS (
	         SELECT 1 FROM pg_catalog.pg_constraint k
	          WHERE k.conrelid OPERATOR(pg_catalog.=) a.attrelid
	            AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) 'n'
	            AND k.conkey[1] OPERATOR(pg_catalog.=) a.attnum
	            AND NOT k.convalidated)
	  FROM pg_catalog.pg_attribute a
	 WHERE a.attrelid OPERATOR(pg_catalog.=) $1
	   AND a.attname OPERATOR(pg_catalog.=) $2
	   AND a.attnum OPERATOR(pg_catalog.>) 0
	   AND NOT a.attisdropped`

// parentCheck is one CHECK constraint of the parent.
type parentCheck struct {
	name string
	expr string
}

// parentChecksSQL lists the parent's CHECK constraints with their
// expressions as the server prints them. Every relation, operator, and
// function is pg_catalog-qualified (CO-9).
const parentChecksSQL = `SELECT conname::pg_catalog.text, pg_catalog.pg_get_expr(conbin, conrelid)
	  FROM pg_catalog.pg_constraint
	 WHERE conrelid OPERATOR(pg_catalog.=) $1
	   AND contype OPERATOR(pg_catalog.=) 'c'
	 ORDER BY conname`

func readParentChecks(ctx context.Context, q querier, parentOID uint32) ([]parentCheck, error) {
	rows, err := q.Query(ctx, parentChecksSQL, parentOID)
	if err != nil {
		return nil, fmt.Errorf("list the CHECK constraints of table %d: %w", parentOID, err)
	}
	checks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (parentCheck, error) {
		var c parentCheck
		err := row.Scan(&c.name, &c.expr)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the CHECK constraints of table %d: %w", parentOID, err)
	}
	return checks, nil
}

// parentNotNullSQL lists the parent's columns that refuse NULL, in column
// order. Every relation and operator is pg_catalog-qualified (CO-9).
const parentNotNullSQL = `SELECT attname::pg_catalog.text
	  FROM pg_catalog.pg_attribute
	 WHERE attrelid OPERATOR(pg_catalog.=) $1
	   AND attnum OPERATOR(pg_catalog.>) 0
	   AND NOT attisdropped
	   AND attnotnull
	 ORDER BY attnum`

func readParentNotNullColumns(ctx context.Context, q querier, parentOID uint32) ([]string, error) {
	rows, err := q.Query(ctx, parentNotNullSQL, parentOID)
	if err != nil {
		return nil, fmt.Errorf("list the NOT NULL columns of table %d: %w", parentOID, err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the NOT NULL columns of table %d: %w", parentOID, err)
	}
	return columns, nil
}
