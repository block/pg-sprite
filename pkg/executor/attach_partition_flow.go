package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// The online ATTACH PARTITION flow. A plain ATTACH scans the new table to
// prove its rows fit the bound, scans the parent's DEFAULT partition to prove
// none of its rows does, builds every index of the parent the table lacks,
// and validates every CHECK, NOT NULL, and foreign key it must create, all
// while it holds ACCESS EXCLUSIVE on the table and the DEFAULT partition.
// This flow does each of those first, while writes go on, with the steps of
// the attach_*.go files, and then runs the reviewed ATTACH, which changes
// only the catalog.

var (
	// ErrAttachPartitionUnsupportedShape is returned when the statement is
	// not one ALTER TABLE schema.parent ATTACH PARTITION table FOR VALUES
	// ...: another statement, a DEFAULT bound, which needs a proof against
	// every other partition's bound, or an unqualified parent.
	ErrAttachPartitionUnsupportedShape = errors.New("the statement is not an ATTACH PARTITION ... FOR VALUES the online attach takes")
	// ErrAttachPartitionTable is returned when the table the statement
	// attaches does not exist, is not a plain table, or is already a
	// partition or inheritance child of another table.
	ErrAttachPartitionTable = errors.New("the table to attach is not a standalone plain table")
	// ErrAttachPartitionDefaultWindow is returned, before anything is
	// changed, when the parent has a DEFAULT partition and the caller did
	// not accept the write window its proof opens: from the moment the
	// proof is added until the attach commits, a write the parent routes to
	// the DEFAULT partition with a key inside the new bound fails with a
	// check violation.
	ErrAttachPartitionDefaultWindow = errors.New("the parent has a DEFAULT partition, and the attach's write window on it was not accepted")
)

// AttachPartitionOptions bounds the online attach.
type AttachPartitionOptions struct {
	// Attach bounds the brief transaction that runs the ATTACH.
	Attach Budget
	// Validate bounds each validation of a proof, CHECK, NOT NULL helper,
	// or foreign key.
	Validate ValidateBudget
	// Index bounds each concurrent build of one of the parent's indexes on
	// the table.
	Index ConcurrentBudget
	// HolderWait is the longest a step that needs ACCESS EXCLUSIVE waits
	// outside the lock queue while a reader or writer holds the table.
	HolderWait time.Duration
	// AcceptDefaultWriteWindow accepts the write window the DEFAULT
	// partition's proof opens (see ErrAttachPartitionDefaultWindow). The
	// flow adds that proof last, right before the attach, to keep the
	// window short.
	AcceptDefaultWriteWindow bool
}

func (o AttachPartitionOptions) validate() error {
	if err := o.Attach.validate(); err != nil {
		return err
	}
	if err := o.Validate.validate(); err != nil {
		return err
	}
	if err := o.Index.validate(); err != nil {
		return err
	}
	if o.HolderWait < 0 || o.HolderWait > maxOverallBudget {
		return fmt.Errorf("lock holder wait must be between 0 and %s, got %s", maxOverallBudget, o.HolderWait)
	}
	return nil
}

// AttachPartitionStepKind is what one step of the attach gave the table, or
// the DEFAULT partition.
type AttachPartitionStepKind string

const (
	// AttachPartitionStepBoundProof is the table's CHECK that implies the
	// bound.
	AttachPartitionStepBoundProof AttachPartitionStepKind = "bound-proof"
	// AttachPartitionStepIndex is one index of the parent.
	AttachPartitionStepIndex AttachPartitionStepKind = "index"
	// AttachPartitionStepCheck is one CHECK constraint of the parent.
	AttachPartitionStepCheck AttachPartitionStepKind = "check"
	// AttachPartitionStepNotNull is one column the parent declares NOT NULL.
	AttachPartitionStepNotNull AttachPartitionStepKind = "not-null"
	// AttachPartitionStepForeignKey is one enforced foreign key of the
	// parent.
	AttachPartitionStepForeignKey AttachPartitionStepKind = "foreign-key"
	// AttachPartitionStepDefaultProof is the DEFAULT partition's CHECK that
	// excludes the bound.
	AttachPartitionStepDefaultProof AttachPartitionStepKind = "default-proof"
)

// AttachPartitionStep is what the flow did for one thing the attach needs.
type AttachPartitionStep struct {
	Kind AttachPartitionStepKind `json:"kind"`
	// Name is the parent's index, constraint, or column, or the proof
	// CHECK's name.
	Name string `json:"name"`
	// Action is what the step did: found, added, validated, built, keyed,
	// or set-not-null.
	Action string `json:"action"`
}

// AttachPartitionReport says what an online attach did.
type AttachPartitionReport struct {
	// ParentSchema and Parent identify the partitioned table.
	ParentSchema string `json:"parent_schema"`
	Parent       string `json:"parent"`
	// Schema and Table identify the attached table.
	Schema string `json:"schema,omitempty"`
	Table  string `json:"table,omitempty"`
	// Bound is the partition's FOR VALUES clause as the server prints it.
	Bound string `json:"bound,omitempty"`
	// Attached is true when this run attached the table, and false when it
	// found it already attached.
	Attached bool `json:"attached"`
	// Steps lists what the run gave the table and the DEFAULT partition, in
	// order, including what it found already in place. It is empty when the
	// table was already attached.
	Steps []AttachPartitionStep `json:"steps,omitempty"`
	// Duration is the wall-clock time of the whole run. It encodes as
	// integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
}

// AttachPartitionOnline runs a reviewed ALTER TABLE ... ATTACH PARTITION ...
// FOR VALUES online, and returns once the table is a partition of the
// parent.
//
// The statement is one ALTER TABLE schema.parent ATTACH PARTITION table FOR
// VALUES ..., on the preflighted partitioned table; an unqualified table name
// resolves under AttemptSearchPath of the parent's schema. The run holds the
// parent's advisory table lock (LK-1), so a second run on the same parent is
// refused with a *dbconn.TableLockHeldError; cfg is the connection target
// that lock's session dials. Under the lock it:
//
//  1. resolves the parent, the table, and the parent's DEFAULT partition,
//     and refuses a table that is not a standalone plain table
//     (ErrAttachPartitionTable), and a DEFAULT partition the caller did not
//     accept the write window of (ErrAttachPartitionDefaultWindow);
//  2. gives the table a validated CHECK that implies the bound, which from
//     then on refuses writes to the table outside it;
//  3. gives the table every CHECK and NOT NULL of the parent, validated;
//  4. gives the table every index of the parent, built concurrently, and
//     its primary key and unique constraints;
//  5. gives the table every enforced foreign key of the parent, validated;
//  6. gives the DEFAULT partition, when there is one, a validated CHECK that
//     excludes the bound;
//  7. runs the reviewed ATTACH, which scans and builds nothing, and drops
//     both proof CHECKs in its transaction.
//
// Every step reads the catalog and decides, so a failure keeps what earlier
// steps finished, and rerunning the same statement resumes; a table found
// already attached has any leftover proof dropped and is reported as found.
func AttachPartitionOnline(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, o AttachPartitionOptions) (AttachPartitionReport, error) {
	return attachPartitionAt(ctx, cfg, pool, pt, sql, o, nil, 0)
}

// AttachPartitionOnlineAsStep is AttachPartitionOnline reported to the run
// step at: the whole attach is step at.Step of a run that owns the tracker,
// and each statement it runs is a statement of that one step. The call never
// calls Start or Finish.
func AttachPartitionOnlineAsStep(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, o AttachPartitionOptions, at RunStep) (AttachPartitionReport, error) {
	if err := at.validate(); err != nil {
		return AttachPartitionReport{}, err
	}
	return attachPartitionAt(ctx, cfg, pool, pt, sql, o, at.Tracker, at.Step)
}

func attachPartitionAt(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, pt preflight.PreflightedTable, sql string, o AttachPartitionOptions, tracker *progress.Tracker, runStep int) (AttachPartitionReport, error) {
	rep := AttachPartitionReport{ParentSchema: pt.Schema(), Parent: pt.Table()}
	if err := o.validate(); err != nil {
		return rep, err
	}
	st, attach, err := admitAttachPartition(sql, pt)
	if err != nil {
		return rep, err
	}
	start := time.Now()
	if tracker != nil {
		start = tracker.Now()
	}
	ran := false
	err = runUnderParentLock(ctx, cfg, pool, pt.Schema(), pt.Table(), "the online attach", func(lockedCtx context.Context) error {
		ran = true
		f := attachPartitionFlow{pool: pool, st: st, attach: attach, searchPath: AttemptSearchPath(pt.Schema(), nil), opts: o, tracker: tracker, runStep: runStep, rep: &rep}
		return f.run(lockedCtx)
	})
	if !ran {
		return rep, err
	}
	rep.Duration = elapsedSince(tracker, start)
	return rep, err
}

// admitAttachPartition is the attach's admission check: the statement's
// shape, on the schema-qualified table the preflight verified, which must be
// partitioned. Nothing is executed on a refusal.
func admitAttachPartition(sql string, pt preflight.PreflightedTable) (statement.Statement, statement.AttachPartition, error) {
	attach, ok, err := statement.AttachedPartition(sql)
	if err != nil {
		return statement.Statement{}, attach, err
	}
	if !ok {
		return statement.Statement{}, attach, fmt.Errorf("%w: not a single ALTER TABLE ... ATTACH PARTITION", ErrAttachPartitionUnsupportedShape)
	}
	if attach.Bound.Default {
		return statement.Statement{}, attach, fmt.Errorf("%w: attaching a DEFAULT partition", ErrAttachPartitionUnsupportedShape)
	}
	if attach.ParentSchema == "" {
		return statement.Statement{}, attach, fmt.Errorf("%w: the partitioned table %s is not schema-qualified", ErrAttachPartitionUnsupportedShape, attach.Parent)
	}
	st, err := statement.ParseOne(sql)
	if err != nil {
		return statement.Statement{}, attach, err
	}
	// INV: ST-7 — the executor runs exactly the statement that was gated, and
	// only against the table the preflight proof verified.
	if attach.ParentSchema != pt.Schema() || attach.Parent != pt.Table() {
		return statement.Statement{}, attach, fmt.Errorf("%w: ST-7: statement targets %q but preflight verified %q",
			ErrInvariantViolation, qualifiedName(attach.ParentSchema, attach.Parent), qualifiedName(pt.Schema(), pt.Table()))
	}
	if !pt.Partitioned() {
		return statement.Statement{}, attach, fmt.Errorf("%w: %s", ErrNotPartitionedTable, qualifiedName(pt.Schema(), pt.Table()))
	}
	return st, attach, nil
}

// attachPartitionFlow is one run of the online attach under the parent's
// advisory lock.
type attachPartitionFlow struct {
	pool       *pgxpool.Pool
	st         statement.Statement
	attach     statement.AttachPartition
	searchPath []string
	opts       AttachPartitionOptions
	tracker    *progress.Tracker
	runStep    int
	rep        *AttachPartitionReport
}

// attachResolved is what the flow resolved under the lock.
type attachResolved struct {
	parentOID uint32
	table     relationFacts
	deflt     relationFacts
}

func (f attachPartitionFlow) run(ctx context.Context) error {
	r, err := f.resolve(ctx)
	if err != nil {
		return err
	}
	table := attachTable{oid: r.table.oid, schema: r.table.schema, name: r.table.name}
	f.rep.Schema, f.rep.Table = table.schema, table.name
	check, exclusion, err := deriveAttachBoundCheck(ctx, briefTx{pool: f.pool}, r.parentOID, f.attach.Bound)
	if err != nil {
		return err
	}
	target := attachPartitionTarget{st: f.st, searchPath: f.searchPath, parentOID: r.parentOID, bound: table.proof(attachProofBound, check, "")}
	if r.deflt.oid != 0 {
		target.deflt = attachProof{kind: attachProofDefault, tableOID: r.deflt.oid, schema: r.deflt.schema, table: r.deflt.name, expr: exclusion}
	}
	var m attachMembership
	err = briefTx{pool: f.pool}.read(ctx, "read the parents of "+table.qualified(), func(ctx context.Context, q querier) error {
		var err error
		m, err = readAttachMembership(ctx, q, target.bound)
		return err
	})
	if err != nil {
		return err
	}
	// A table already attached to the parent, by an earlier run or by
	// someone else, needs only its leftover proofs dropped, which the attach
	// step does.
	if !m.isPartitionOf(r.parentOID) {
		if err := f.prepare(ctx, r.parentOID, m, table, target); err != nil {
			return err
		}
	}
	res, err := attachPartitionOnline(ctx, f.pool, target, f.opts.Attach, f.opts.HolderWait, f.tracker, f.started)
	if err != nil {
		return err
	}
	f.rep.Bound, f.rep.Attached = res.bound, res.action == attachPartitionAttached
	return nil
}

// resolve resolves the parent, the table as the statement spells it under
// the attempt's search_path, and the parent's DEFAULT partition, and refuses
// a table that is not a plain table.
func (f attachPartitionFlow) resolve(ctx context.Context) (attachResolved, error) {
	var r attachResolved
	err := briefTx{pool: f.pool}.run(ctx, "resolve the tables of the attach", f.searchPath, false, func(ctx context.Context, tx pgx.Tx) error {
		parentOID, err := resolveAttachName(ctx, tx, f.attach.ParentSchema, f.attach.Parent)
		if err != nil {
			return err
		}
		if parentOID == 0 {
			return fmt.Errorf("%w: %s does not exist", ErrNotPartitionedTable, qualifiedName(f.attach.ParentSchema, f.attach.Parent))
		}
		parent, found, err := relationByOID(ctx, tx, parentOID)
		if err != nil {
			return err
		}
		if !found || parent.relkind != "p" {
			return fmt.Errorf("%w: %s is not a partitioned table when the catalog is read under the table lock",
				ErrNotPartitionedTable, qualifiedName(f.attach.ParentSchema, f.attach.Parent))
		}
		r.parentOID = parentOID
		tableOID, err := resolveAttachName(ctx, tx, f.attach.Schema, f.attach.Name)
		if err != nil {
			return err
		}
		spelled := qualifiedName(f.attach.Schema, f.attach.Name)
		if tableOID == 0 {
			return fmt.Errorf("%w: %s does not exist", ErrAttachPartitionTable, spelled)
		}
		if r.table, found, err = relationByOID(ctx, tx, tableOID); err != nil {
			return err
		}
		if !found || r.table.relkind != "r" {
			return fmt.Errorf("%w: %s is of kind %q, and the online attach takes a plain table", ErrAttachPartitionTable, spelled, r.table.relkind)
		}
		var defaultOID uint32
		if err := tx.QueryRow(ctx, attachDefaultPartitionSQL, parentOID).Scan(&defaultOID); err != nil {
			return fmt.Errorf("read the DEFAULT partition of %s: %w", qualifiedName(parent.schema, parent.name), err)
		}
		if defaultOID != 0 {
			if r.deflt, found, err = relationByOID(ctx, tx, defaultOID); err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: ST-7: the DEFAULT partition %d of %s vanished", ErrInvariantViolation, defaultOID, qualifiedName(parent.schema, parent.name))
			}
		}
		return nil
	})
	return r, err
}

// resolveAttachName resolves a name as the statement spells it, 0 for none.
func resolveAttachName(ctx context.Context, tx pgx.Tx, schema, name string) (uint32, error) {
	spelled := pgx.Identifier{name}
	if schema != "" {
		spelled = pgx.Identifier{schema, name}
	}
	var oid uint32
	if err := tx.QueryRow(ctx, attachResolveSQL, spelled.Sanitize()).Scan(&oid); err != nil {
		return 0, fmt.Errorf("resolve %s: %w", spelled.Sanitize(), err)
	}
	return oid, nil
}

// prepare gives the standalone table, and the DEFAULT partition, everything
// the attach needs to change only the catalog.
func (f attachPartitionFlow) prepare(ctx context.Context, parentOID uint32, m attachMembership, table attachTable, target attachPartitionTarget) error {
	if m.isPartition || len(m.parents) > 0 {
		return fmt.Errorf("%w: %s is a partition or inheritance child of table %v", ErrAttachPartitionTable, table.qualified(), m.parents)
	}
	if target.hasDefault() && !f.opts.AcceptDefaultWriteWindow {
		return fmt.Errorf("%w: %s; until the attach commits, writes it receives inside the new bound would fail",
			ErrAttachPartitionDefaultWindow, target.deflt.qualified())
	}
	proof, err := ensureAttachProof(ctx, f.pool, target.bound, f.opts.Validate, f.opts.HolderWait, f.tracker, f.started)
	if err != nil {
		return err
	}
	f.record(AttachPartitionStepBoundProof, proof.constraint, string(proof.action))
	// The NOT NULLs go before the indexes: a primary key's step refuses a
	// key column that allows NULL, because setting it NOT NULL there would
	// scan the table under ACCESS EXCLUSIVE.
	constraints, err := ensureAttachConstraints(ctx, f.pool, parentOID, table, f.opts.Validate, f.opts.HolderWait, f.tracker, f.started)
	for _, c := range constraints {
		if c.check != "" {
			f.record(AttachPartitionStepCheck, c.check, string(c.action))
		} else {
			f.record(AttachPartitionStepNotNull, c.column, string(c.action))
		}
	}
	if err != nil {
		return err
	}
	node := standaloneTable(table.oid, table.schema, table.name)
	indexes, err := ensureAttachIndexes(ctx, f.pool, parentOID, node, f.opts.Index)
	for _, ix := range indexes {
		f.record(AttachPartitionStepIndex, ix.parentIndex, string(ix.action))
	}
	if err != nil {
		return err
	}
	fks, err := ensureAttachForeignKeys(ctx, f.pool, parentOID, node, f.opts.Validate, f.started)
	for _, fk := range fks {
		f.record(AttachPartitionStepForeignKey, fk.parentFK, string(fk.action))
	}
	if err != nil {
		return err
	}
	if target.hasDefault() {
		// The DEFAULT partition's proof goes last: it refuses the writes the
		// parent routes there inside the bound until the attach commits.
		proof, err := ensureAttachProof(ctx, f.pool, target.deflt, f.opts.Validate, f.opts.HolderWait, f.tracker, f.started)
		if err != nil {
			return err
		}
		f.record(AttachPartitionStepDefaultProof, proof.constraint, string(proof.action))
	}
	return nil
}

func (f attachPartitionFlow) record(kind AttachPartitionStepKind, name, action string) {
	f.rep.Steps = append(f.rep.Steps, AttachPartitionStep{Kind: kind, Name: name, Action: action})
}

// started reports the statement in flight on the run's step.
func (f attachPartitionFlow) started(op progress.Operation, sql string) {
	if f.tracker != nil {
		f.tracker.StartStep(f.runStep, op, sql)
	}
}
