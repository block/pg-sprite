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

// attachProofNamePrefix starts the name of every proof CHECK whose name the
// step derives, so an operator can tell them from the table's own
// constraints. A parent's CHECK carries the parent's name instead, because
// the attach matches it by name.
const attachProofNamePrefix = "pgsprite_attach_"

// attachProofDigestHexLen is how many hex characters of the expression's
// digest the name carries.
const attachProofDigestHexLen = 16

var (
	// errAttachProofNameTaken is returned when the proof's name is held by
	// a constraint that is not a CHECK; for a parent's CHECK or a not-null
	// helper, by a CHECK of another expression; or, for a parent's CHECK,
	// by a NO INHERIT one, which the attach refuses to merge with the
	// parent's. The step drops and renames nothing it did not create.
	errAttachProofNameTaken = errors.New("the name the attach proof needs is held by a constraint that is not that CHECK")
	// errAttachProofViolation is returned when the proof's validation finds
	// rows that break it: rows of the attached table outside the bound, or
	// rows of the DEFAULT partition inside it. The bound proof stays NOT
	// VALID and enforced for new rows; the DEFAULT partition's is dropped.
	errAttachProofViolation = errors.New("rows break the attach proof")
	// errAttachProofNotPlainTable is returned when the table the proof goes
	// on is not a plain table, such as a partitioned one. The schema is
	// valid, and the step does not prove it online.
	errAttachProofNotPlainTable = errors.New("the attach proof goes on a plain table")
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
	// NULL as the server prints it.
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
// each prove the table and decide again under its lock. Before the ADD
// requests its lock, the step waits outside the lock queue, up to
// holderWait, while a reader or writer holds the table, and reports the
// wait to tracker, which may be nil. The VALIDATE is bounded by b. Rows that
// break the DEFAULT partition's proof get its CHECK dropped again, so the
// writes it would refuse go on; the bound proof's CHECK stays. The caller
// holds the parent's advisory table lock (LK-1).
func ensureAttachProof(ctx context.Context, pool *pgxpool.Pool, proof attachProof, b ValidateBudget, holderWait time.Duration, tracker *progress.Tracker, started attachProofStatementStarted) (attachProofResult, error) {
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
		// INV: LK-2 — the ADD takes ACCESS EXCLUSIVE, so it requests its
		// lock once no long-lived reader or writer holds the table, or once
		// holderWait runs out, rather than queue every later statement on
		// the table behind it. Unlike a brief step's awaitLockQueue, a
		// holder that outlasts the wait does not end the proof step: the
		// ADD still requests its lock under its lock budget, so the
		// outcome is not read.
		if _, err := waitOutsideLockQueue(ctx, pool, proof.lockedTables(), holderWait, systemHolderWaitClock(), trackerHolderWaitReport(tracker)); err != nil {
			return attachProofResult{}, err
		}
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
	err = validateAttachProof(ctx, pool, proof, result.constraintOID, b, started)
	if errors.Is(err, errAttachProofViolation) {
		return attachProofResult{}, attachProofViolationOutcome(ctx, pool, proof, holderWait, tracker, err)
	}
	if err != nil {
		return attachProofResult{}, err
	}
	return result, nil
}

// lockedTables is the one table the proof's ADD and DROP lock, with the
// reads and writes their ACCESS EXCLUSIVE conflicts with. ONLY: neither
// statement locks the table's partitions.
func (p attachProof) lockedTables() []lockedTable {
	return []lockedTable{{schema: p.schema, table: p.table, modes: readAndWriteLockModes()}}
}

// attachProofViolationOutcome says what the rows that broke the proof leave
// and what the operator does next. The bound proof's CHECK stays, refusing
// only rows the attach would refuse, and the rerun validates it. The
// DEFAULT partition's is dropped, because until the attach it refuses every
// write the parent routes there for the new bound.
func attachProofViolationOutcome(ctx context.Context, pool *pgxpool.Pool, proof attachProof, holderWait time.Duration, tracker *progress.Tracker, violation error) error {
	// The proofs on the table being attached stay: each refuses only rows
	// the attach would refuse too. Only the DEFAULT partition's is dropped.
	switch proof.kind {
	case attachProofBound:
		return fmt.Errorf("rows of %s fall outside the new partition's bound; its CHECK %s stays NOT VALID and refuses new rows outside the bound; fix or move those rows, then rerun: %w",
			proof.qualified(), proof.name(), violation)
	case attachProofParent:
		return fmt.Errorf("rows of %s break the parent's CHECK %s, which stays NOT VALID on it and refuses new rows that break it; fix those rows, then rerun: %w",
			proof.qualified(), proof.name(), violation)
	case attachProofNotNull:
		return fmt.Errorf("rows of %s hold NULL in a column the parent declares NOT NULL; the CHECK %s stays NOT VALID and refuses new NULLs; fix those rows, then rerun: %w",
			proof.qualified(), proof.name(), violation)
	}
	if err := dropAttachProof(ctx, pool, proof, holderWait, tracker); err != nil {
		return fmt.Errorf("rows of the DEFAULT partition %s fall inside the new partition's bound, and dropping its CHECK %s failed, so it still refuses new rows for the bound; drop it, move those rows, then rerun: %w (drop: %w)",
			proof.qualified(), proof.name(), violation, err)
	}
	return fmt.Errorf("rows of the DEFAULT partition %s fall inside the new partition's bound; its CHECK %s was dropped, so its writes go on; move those rows into the new table, then rerun: %w",
		proof.qualified(), proof.name(), violation)
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
			return fmt.Errorf("%w: %w", errAttachProofViolation, err)
		}
		if validationStopped(err) {
			return fmt.Errorf("validation of attach proof %s on %s stopped; the CHECK stays NOT VALID and refuses new rows that break it until a rerun validates it or it is dropped: %w",
				proof.name(), proof.qualified(), err)
		}
		return fmt.Errorf("validate attach proof %s on %s: %w", proof.name(), proof.qualified(), err)
	}
	return nil
}

// validationStopped reports whether a VALIDATE ended on its budget or a
// cancel rather than on the rows, which leaves its CHECK NOT VALID.
func validationStopped(err error) bool {
	if _, budget := errors.AsType[*BudgetError](err); budget {
		return true
	}
	if errors.Is(err, ErrCancelledByCaller) {
		return true
	}
	return errors.Is(err, ErrCancelledExternally)
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
		return fmt.Errorf("%w: %s is of kind %q", errAttachProofNotPlainTable, proof.qualified(), relkind)
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
	var c attachProofConstraint
	err := q.QueryRow(ctx, attachProofConstraintSQL, proof.tableOID, proof.name()).Scan(&c.oid, &c.contype, &c.validated, &c.noInherit, &c.expr)
	if errors.Is(err, pgx.ErrNoRows) {
		return attachProofAddNeeded, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read attach proof %s on %s: %w", proof.name(), proof.qualified(), err)
	}
	return decideAttachProof(proof, c)
}

// attachProofConstraint is the constraint that holds a proof's name.
type attachProofConstraint struct {
	oid       uint32
	contype   string
	validated bool
	noInherit bool
	// expr is the expression as the server prints it (pg_get_expr).
	expr string
}

// decideAttachProof decides from the constraint that holds the name. A
// parent's CHECK and a not-null helper must also carry the proof's
// expression as the server prints it: the parent's is printed against a
// table whose columns the attach requires to match by name and type, and
// the helper's is the server's own print of it, so the same expression
// prints the same. A parent's CHECK must also be inheritable, or the attach
// refuses to merge it with the parent's.
func decideAttachProof(proof attachProof, c attachProofConstraint) (attachProofState, uint32, error) {
	if c.contype != "c" {
		return 0, 0, fmt.Errorf("%w: %s on %s is a constraint of type %q", errAttachProofNameTaken, proof.name(), proof.qualified(), c.contype)
	}
	if proof.comparesExpression() && c.expr != proof.expr {
		return 0, 0, fmt.Errorf("%w: CHECK %s on %s is (%s), and the proof is (%s)",
			errAttachProofNameTaken, proof.name(), proof.qualified(), c.expr, proof.expr)
	}
	if proof.kind == attachProofParent && c.noInherit {
		return 0, 0, fmt.Errorf("%w: CHECK %s on %s is NO INHERIT, which the attach does not merge with the parent's",
			errAttachProofNameTaken, proof.name(), proof.qualified())
	}
	if c.validated {
		return attachProofDone, c.oid, nil
	}
	return attachProofValidatePending, c.oid, nil
}

// comparesExpression reports whether a CHECK under the proof's name must
// carry the proof's expression exactly. A bound or DEFAULT proof's
// expression is rendered by the engine, which the server prints
// differently, so the text cannot be compared; its derived name changes
// with the expression.
func (p attachProof) comparesExpression() bool {
	return p.kind == attachProofParent || p.kind == attachProofNotNull
}

// attachProofConstraintSQL reads the constraint under a name on a table.
// Every relation and operator is pg_catalog-qualified (CO-9).
const attachProofConstraintSQL = `SELECT oid, contype::pg_catalog.text, convalidated, connoinherit,
	       COALESCE(pg_catalog.pg_get_expr(conbin, conrelid), '')
	FROM pg_catalog.pg_constraint
	WHERE conrelid OPERATOR(pg_catalog.=) $1 AND conname OPERATOR(pg_catalog.=) $2`
