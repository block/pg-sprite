package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// The proof step of an online ATTACH PARTITION. A plain ATTACH scans the
// attached table, and the parent's DEFAULT partition, under ACCESS
// EXCLUSIVE; a validated CHECK that implies the bound, or the DEFAULT
// partition's exclusion of it, lets the server skip each scan
// (attach_bound_check.go renders the two expressions). This file brings one
// table to such a CHECK online: ADD CONSTRAINT ... CHECK ... NOT VALID, a
// brief catalog change, then VALIDATE CONSTRAINT, which scans under SHARE
// UPDATE EXCLUSIVE while writes go on.
//
// A NOT VALID CHECK is enforced for new rows from the moment it is added.
// On the attached table that refuses only rows the attach would refuse. On
// the DEFAULT partition it refuses, until the attach, every write the parent
// would route there for the new bound, so the caller adds the DEFAULT
// partition's proof as late as it can and attaches as soon as it is
// validated.

// attachProofKind is which proof a CHECK carries.
type attachProofKind string

const (
	// attachProofBound: on the attached table, its rows fit the bound.
	attachProofBound attachProofKind = "bound"
	// attachProofDefault: on the DEFAULT partition, none of its rows
	// belongs to the bound.
	attachProofDefault attachProofKind = "default"
	// attachProofParent: on the attached table, a CHECK constraint of the
	// parent, under the parent's name, which the attach requires the table
	// to carry validated.
	attachProofParent attachProofKind = "parent"
	// attachProofNotNull: on the attached table, a column holds no NULL,
	// which lets SET NOT NULL skip its scan.
	attachProofNotNull attachProofKind = "notnull"
)

// attachProofNamePrefix starts the name of every proof CHECK the step adds,
// so an operator can tell them from the table's own constraints.
const attachProofNamePrefix = "pgsprite_attach_"

// attachProofDigestHexLen is how many hex characters of the expression's
// digest the name carries.
const attachProofDigestHexLen = 16

var (
	// errAttachProofNameTaken is returned when the proof's name is held by
	// a constraint that is not a CHECK, or, for a parent's CHECK, by a
	// CHECK of another expression. The step drops and renames nothing it
	// did not create.
	errAttachProofNameTaken = errors.New("the name the attach proof needs is held by a constraint that is not that CHECK")
	// errAttachProofViolation is returned when the proof's validation finds
	// rows that break it: rows of the attached table outside the bound, or
	// rows of the DEFAULT partition inside it. The CHECK stays NOT VALID and
	// enforced for new rows.
	errAttachProofViolation = errors.New("rows break the attach proof")
)

// sqlstateCheckConstraintViolation is check_violation.
const sqlstateCheckConstraintViolation = "23514"

// attachProof is one proof CHECK the attach needs on one table.
type attachProof struct {
	kind attachProofKind
	// The table the CHECK goes on, as resolved when the run began: the
	// attached table, or the parent's DEFAULT partition.
	tableOID uint32
	schema   string
	table    string
	// expr is the CHECK's expression: as attachBoundCheck rendered it, as
	// the server printed the parent's (pg_get_expr), or a column's IS NOT
	// NULL.
	expr string
	// constraint is the parent's constraint name, for attachProofParent
	// only; every other kind derives its name.
	constraint string
}

func (p attachProof) qualified() string { return qualifiedName(p.schema, p.table) }

// name is the proof's constraint name. A parent's CHECK keeps the parent's
// name, which the attach matches by. Every other proof is the prefix, the
// kind, and a digest of the expression: deterministic, so a rerun finds the
// CHECK an earlier run added, and changing with the expression, so a CHECK
// of another bound or column is never taken for this one.
func (p attachProof) name() string {
	if p.kind == attachProofParent {
		return p.constraint
	}
	digest := sha256.Sum256([]byte(p.expr))
	return attachProofNamePrefix + string(p.kind) + "_" + hex.EncodeToString(digest[:])[:attachProofDigestHexLen]
}

// validate refuses a proof the step cannot act on.
func (p attachProof) validate() error {
	switch p.kind {
	case attachProofBound, attachProofDefault, attachProofParent, attachProofNotNull:
	default:
		return fmt.Errorf("%w: unknown attach proof kind %q", ErrInvariantViolation, p.kind)
	}
	if p.tableOID == 0 || p.schema == "" || p.table == "" {
		return fmt.Errorf("%w: the attach proof names no resolved table", ErrInvariantViolation)
	}
	if p.expr == "" {
		return fmt.Errorf("%w: the attach proof on %s has no expression", ErrInvariantViolation, p.qualified())
	}
	if (p.kind == attachProofParent) != (p.constraint != "") {
		return fmt.Errorf("%w: attach proof of kind %q on %s with constraint name %q",
			ErrInvariantViolation, p.kind, p.qualified(), p.constraint)
	}
	return nil
}

func (p attachProof) addSQL() string {
	return "ALTER TABLE " + pgx.Identifier{p.schema, p.table}.Sanitize() +
		" ADD CONSTRAINT " + pgx.Identifier{p.name()}.Sanitize() + " CHECK (" + p.expr + ") NOT VALID"
}

func (p attachProof) validateSQL() string {
	return "ALTER TABLE " + pgx.Identifier{p.schema, p.table}.Sanitize() +
		" VALIDATE CONSTRAINT " + pgx.Identifier{p.name()}.Sanitize()
}

// attachProofState is what the table holds under the proof's name.
type attachProofState int

const (
	// attachProofAddNeeded: nothing holds the name.
	attachProofAddNeeded attachProofState = iota + 1
	// attachProofValidatePending: a NOT VALID CHECK holds it.
	attachProofValidatePending
	// attachProofDone: a validated CHECK holds it.
	attachProofDone
)

// attachProofAction says what the step did.
type attachProofAction string

const (
	// attachProofFound: the table already had the validated CHECK.
	attachProofFound attachProofAction = "found"
	// attachProofValidated: the CHECK was NOT VALID, and the step validated it.
	attachProofValidated attachProofAction = "validated"
	// attachProofAdded: the step added the CHECK NOT VALID, then validated it.
	attachProofAdded attachProofAction = "added"
)

// attachProofResult is the table's proof CHECK once the step is done.
type attachProofResult struct {
	action        attachProofAction
	constraint    string
	constraintOID uint32
}

// attachProofStatementStarted is told each statement the step is about to
// run, with its progress operation. It may be nil.
type attachProofStatementStarted func(op progress.Operation, sql string)

// ensureAttachProof brings one table to the proof's validated CHECK. It
// reads the table and decides every call, so a rerun resumes from whatever
// an earlier call left: a validated CHECK under the proof's name is the
// result as it stands, a NOT VALID one is validated, and otherwise the CHECK
// is added NOT VALID, then validated. A name held by another kind of
// constraint is refused before anything is added. The ADD and the VALIDATE
// each prove the table and decide again under its lock. The VALIDATE is
// bounded by b. The caller holds the parent's advisory table lock (LK-1).
func ensureAttachProof(ctx context.Context, pool *pgxpool.Pool, proof attachProof, b ValidateBudget, started attachProofStatementStarted) (attachProofResult, error) {
	if err := proof.validate(); err != nil {
		return attachProofResult{}, err
	}
	if err := b.validate(); err != nil {
		return attachProofResult{}, err
	}
	tx := briefTx{pool: pool}
	var state attachProofState
	var oid uint32
	err := tx.read(ctx, "inspect the attach proof of "+proof.qualified(), func(ctx context.Context, q querier) error {
		var err error
		state, oid, err = readAttachProof(ctx, q, proof)
		return err
	})
	if err != nil {
		return attachProofResult{}, err
	}
	result := attachProofResult{constraint: proof.name(), constraintOID: oid}
	switch state {
	case attachProofDone:
		result.action = attachProofFound
		return result, nil
	case attachProofValidatePending:
		result.action = attachProofValidated
	case attachProofAddNeeded:
		reportAttachProofStatement(started, progress.OperationBrief, proof.addSQL())
		ended, addedOID, added, err := addAttachProof(ctx, tx, proof)
		if err != nil {
			return attachProofResult{}, fmt.Errorf("add attach proof %s on %s: %w", proof.name(), proof.qualified(), err)
		}
		result.constraintOID = addedOID
		switch {
		case ended == attachProofDone:
			// A validated CHECK landed between the read and the lock.
			result.action = attachProofFound
			return result, nil
		case added:
			result.action = attachProofAdded
		default:
			result.action = attachProofValidated
		}
	default:
		return attachProofResult{}, fmt.Errorf("%w: unknown attach proof state %d", ErrInvariantViolation, state)
	}
	if err := validateAttachProof(ctx, pool, proof, result.constraintOID, b, started); err != nil {
		return attachProofResult{}, err
	}
	return result, nil
}

func reportAttachProofStatement(started attachProofStatementStarted, op progress.Operation, sql string) {
	if started != nil {
		started(op, sql)
	}
}

// addAttachProof runs the ADD CONSTRAINT ... CHECK ... NOT VALID in one
// brief transaction. Under the table's lock it proves the table and decides
// again: a CHECK under the name that landed since the first read is taken as
// it stands, and nothing is added. The ADD takes ACCESS EXCLUSIVE for the
// transaction and scans nothing. Before the commit it reads back a NOT VALID
// CHECK under the name. It returns the state it ends on, the CHECK's OID,
// and whether it ran the ADD.
func addAttachProof(ctx context.Context, tx briefTx, proof attachProof) (state attachProofState, oid uint32, added bool, err error) {
	err = tx.run(ctx, "add the attach proof of "+proof.qualified(), catalogReadPath(), true,
		func(ctx context.Context, tx pgx.Tx) error {
			// INV: ST-7 — the table stays under its name, and its
			// constraints stay as read, until the transaction ends.
			if err := lockAndProveAttachProofTable(ctx, tx, proof); err != nil {
				return err
			}
			var err error
			if state, oid, err = readAttachProof(ctx, tx, proof); err != nil {
				return err
			}
			if state != attachProofAddNeeded {
				return nil
			}
			// INV: LK-2 — NOT VALID skips the scan, so the statement
			// changes only the catalog, under the pool's lock_timeout and
			// statement_timeout.
			if _, err := tx.Exec(ctx, proof.addSQL()); err != nil {
				return err
			}
			added = true
			if state, oid, err = readAttachProof(ctx, tx, proof); err != nil {
				return err
			}
			if state != attachProofValidatePending {
				return fmt.Errorf("%w: ST-7: attach proof %s on %s is in state %d after its ADD, want NOT VALID",
					ErrInvariantViolation, proof.name(), proof.qualified(), state)
			}
			return nil
		})
	return state, oid, added, err
}

// validateAttachProof runs the VALIDATE CONSTRAINT under b: the lock wait
// bounded by b.LockTimeout and the scan by b.Overall. VALIDATE takes SHARE
// UPDATE EXCLUSIVE, which lets reads and writes go on. Under that lock and
// before the statement, it proves the table and that the CHECK it decided on
// is still there, NOT VALID; after the statement, before the commit, that
// it is validated. Rows that break the CHECK are errAttachProofViolation.
func validateAttachProof(ctx context.Context, pool *pgxpool.Pool, proof attachProof, oid uint32, b ValidateBudget, started attachProofStatementStarted) error {
	validateSQL := proof.validateSQL()
	st, err := statement.ParseOne(validateSQL)
	if err != nil {
		return fmt.Errorf("parse the validation of attach proof %s on %s: %w", proof.name(), proof.qualified(), err)
	}
	before := func(ctx context.Context, tx pgx.Tx) error {
		// INV: ST-7 — the lock VALIDATE takes, taken first, holds the
		// table and its constraints until the transaction ends.
		if err := lockAndProveAttachProofTable(ctx, tx, proof); err != nil {
			return err
		}
		return requireAttachProofState(ctx, tx, proof, oid, attachProofValidatePending)
	}
	after := func(ctx context.Context, tx pgx.Tx) error {
		return requireAttachProofState(ctx, tx, proof, oid, attachProofDone)
	}
	reportAttachProofStatement(started, progress.OperationValidate, validateSQL)
	start := time.Now()
	err = executeBoundedAttemptAs(ctx, pool, st, Budget{LockTimeout: b.LockTimeout, StatementTimeout: b.Overall},
		catalogReadPath(), "", before, after)
	if err = corroborateValidateCancel(ctx, err, b, time.Since(start)); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateCheckConstraintViolation {
			return fmt.Errorf("%w: %s proof %s on %s stays NOT VALID and enforced for new rows: %w",
				errAttachProofViolation, proof.kind, proof.name(), proof.qualified(), err)
		}
		return fmt.Errorf("validate attach proof %s on %s: %w", proof.name(), proof.qualified(), err)
	}
	return nil
}

// requireAttachProofState decides again and requires want on the CHECK with
// the given OID.
func requireAttachProofState(ctx context.Context, q querier, proof attachProof, oid uint32, want attachProofState) error {
	state, found, err := readAttachProof(ctx, q, proof)
	if err != nil {
		return err
	}
	if state != want || found != oid {
		return fmt.Errorf("%w: ST-7: attach proof %s on %s is constraint %d in state %d, want constraint %d in state %d",
			ErrInvariantViolation, proof.name(), proof.qualified(), found, state, oid, want)
	}
	return nil
}

// lockAndProveAttachProofTable takes SHARE UPDATE EXCLUSIVE on the table,
// under the transaction's lock_timeout (LK-2), and proves under it that the
// name still resolves to the table the run resolved, a plain table. The
// lock excludes every other constraint change, a rename, and a detach until
// the transaction ends, and does not block the table's writers.
func lockAndProveAttachProofTable(ctx context.Context, tx pgx.Tx, proof attachProof) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{proof.schema, proof.table}.Sanitize()+" IN SHARE UPDATE EXCLUSIVE MODE")
	if err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
			return fmt.Errorf("%w: ST-7: table %s no longer exists under that name; it was renamed or dropped since the run resolved it",
				ErrInvariantViolation, proof.qualified())
		}
		return err
	}
	var oid uint32
	var relkind string
	err = tx.QueryRow(ctx, attachProofTableSQL, proof.schema, proof.table).Scan(&oid, &relkind)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && oid != proof.tableOID) {
		return fmt.Errorf("%w: ST-7: the name %s no longer resolves to table %d", ErrInvariantViolation, proof.qualified(), proof.tableOID)
	}
	if err != nil {
		return fmt.Errorf("prove table %s: %w", proof.qualified(), err)
	}
	if relkind != "r" {
		return fmt.Errorf("%w: %s is of kind %q, and an attach proof goes on a plain table", ErrInvariantViolation, proof.qualified(), relkind)
	}
	return nil
}

// attachProofTableSQL resolves a table by name. Every relation and operator
// is pg_catalog-qualified (CO-9).
const attachProofTableSQL = `SELECT c.oid, c.relkind::pg_catalog.text
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
	WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`

// readAttachProof reads what holds the proof's name on its table, and the
// constraint's OID when one does.
func readAttachProof(ctx context.Context, q querier, proof attachProof) (attachProofState, uint32, error) {
	var oid uint32
	var contype, expr string
	var validated bool
	err := q.QueryRow(ctx, attachProofConstraintSQL, proof.tableOID, proof.name()).Scan(&oid, &contype, &validated, &expr)
	if errors.Is(err, pgx.ErrNoRows) {
		return attachProofAddNeeded, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read attach proof %s on %s: %w", proof.name(), proof.qualified(), err)
	}
	return decideAttachProof(proof, contype, expr, validated, oid)
}

// decideAttachProof decides from the constraint that holds the name. A
// parent's CHECK must also carry the parent's expression: the server prints
// both against tables whose columns the attach requires to match by name
// and type, so the same expression prints the same.
func decideAttachProof(proof attachProof, contype, expr string, validated bool, oid uint32) (attachProofState, uint32, error) {
	if contype != "c" {
		return 0, 0, fmt.Errorf("%w: %s on %s is a constraint of type %q", errAttachProofNameTaken, proof.name(), proof.qualified(), contype)
	}
	if proof.kind == attachProofParent && expr != proof.expr {
		return 0, 0, fmt.Errorf("%w: CHECK %s on %s is (%s), and the parent's is (%s)",
			errAttachProofNameTaken, proof.name(), proof.qualified(), expr, proof.expr)
	}
	if validated {
		return attachProofDone, oid, nil
	}
	return attachProofValidatePending, oid, nil
}

// attachProofConstraintSQL reads the constraint under a name on a table.
// Every relation and operator is pg_catalog-qualified (CO-9).
const attachProofConstraintSQL = `SELECT oid, contype::pg_catalog.text, convalidated,
	       COALESCE(pg_catalog.pg_get_expr(conbin, conrelid), '')
	FROM pg_catalog.pg_constraint
	WHERE conrelid OPERATOR(pg_catalog.=) $1 AND conname OPERATOR(pg_catalog.=) $2`
