package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// The attach step of an online ATTACH PARTITION. Once the table carries a
// validated CHECK that implies the bound, and the parent's DEFAULT
// partition, when there is one, a validated CHECK that excludes it
// (ensureAttachProof), the ATTACH skips both scans. Once the table also
// carries the parent's indexes, constraints, and foreign keys, it builds and
// validates nothing either, and changes only the catalog. This file runs
// that ATTACH in one brief transaction under the run's lock and statement
// budgets, and drops the two proof CHECKs in the same transaction, under the
// locks the ATTACH already holds, so they never outlive it. Anything the
// table still lacks makes the ATTACH scan or build under its locks, for no
// longer than the statement budget: past it, the attempt ends and rolls
// back, rather than holding the locks for the whole scan.

// errAttachPartitionNotReady is returned when the table, or the parent's
// DEFAULT partition, lacks the validated proof CHECK the attach needs to
// skip its scan, or the parent's DEFAULT partition is not the one the run
// proved. Nothing is attached.
var errAttachPartitionNotReady = errors.New("the attach is not proven to skip its scans")

// attachPartitionAction says what the step did.
type attachPartitionAction string

const (
	// attachPartitionFound: the table was already a partition of the parent.
	attachPartitionFound attachPartitionAction = "found"
	// attachPartitionAttached: the step attached it.
	attachPartitionAttached attachPartitionAction = "attached"
)

// attachPartitionResult is the table once the step is done: a partition of
// the parent, with the bound the catalog records for it.
type attachPartitionResult struct {
	action attachPartitionAction
	// bound is the partition's FOR VALUES clause as the server prints it.
	bound string
}

// attachPartitionTarget is the reviewed ATTACH and what the run resolved
// for it.
type attachPartitionTarget struct {
	// st is the reviewed ATTACH PARTITION, run as it was reviewed, under
	// searchPath.
	st         statement.Statement
	searchPath []string
	parentOID  uint32
	// bound is the proof on the table being attached. Its table is that
	// table.
	bound attachProof
	// deflt is the proof on the parent's DEFAULT partition, or the zero
	// value when the parent has none.
	deflt attachProof
}

// hasDefault reports whether the run proved a DEFAULT partition.
func (t attachPartitionTarget) hasDefault() bool { return t.deflt.tableOID != 0 }

// proofs are the proof CHECKs the attach stands on.
func (t attachPartitionTarget) proofs() []attachProof {
	if t.hasDefault() {
		return []attachProof{t.bound, t.deflt}
	}
	return []attachProof{t.bound}
}

// validate refuses a target the step cannot act on.
func (t attachPartitionTarget) validate() error {
	if t.parentOID == 0 {
		return fmt.Errorf("%w: the attach names no resolved parent", ErrInvariantViolation)
	}
	if t.bound.kind != attachProofBound {
		return fmt.Errorf("%w: the attach's table proof is of kind %q", ErrInvariantViolation, t.bound.kind)
	}
	if t.hasDefault() && t.deflt.kind != attachProofDefault {
		return fmt.Errorf("%w: the attach's DEFAULT partition proof is of kind %q", ErrInvariantViolation, t.deflt.kind)
	}
	for _, p := range t.proofs() {
		if err := p.validate(); err != nil {
			return err
		}
	}
	attach, ok, err := statement.AttachedPartition(t.st.SQL())
	if err != nil {
		return fmt.Errorf("parse the attach: %w", err)
	}
	if !ok || attach.Bound.Default {
		return fmt.Errorf("%w: the attach step takes an ATTACH PARTITION ... FOR VALUES", ErrInvariantViolation)
	}
	return nil
}

// attachPartitionOnline attaches the table to the parent, reading and
// deciding every call, so a rerun resumes:
//
//   - a table that is already a partition of the parent is found as it
//     stands, and any proof CHECK still on it, or on the DEFAULT partition,
//     is dropped;
//   - a standalone table whose proofs are validated, under a parent whose
//     DEFAULT partition is the one proved, is attached;
//   - anything else is refused before any lock is requested.
//
// Before the ATTACH requests its locks, the step waits outside the lock
// queue, up to holderWait, while a reader or writer holds the table or the
// DEFAULT partition, and reports the wait to tracker, which may be nil. The
// transaction then takes the ATTACH's locks in the server's own order,
// proves under them that the statement's names resolve to the tables the
// run resolved, that the DEFAULT partition is the proved one, and that both
// proofs are validated, runs the ATTACH, proves the table a partition of the
// parent, and drops the proofs, all under b. The caller holds the parent's
// advisory table lock (LK-1).
func attachPartitionOnline(ctx context.Context, pool *pgxpool.Pool, t attachPartitionTarget, b Budget,
	holderWait time.Duration, tracker *progress.Tracker, started attachProofStatementStarted) (attachPartitionResult, error) {
	if err := t.validate(); err != nil {
		return attachPartitionResult{}, err
	}
	if err := b.validate(); err != nil {
		return attachPartitionResult{}, err
	}
	tx := briefTx{pool: pool}
	var parent relationFacts
	var membership attachMembership
	err := tx.read(ctx, "inspect the attach of "+t.bound.qualified(), func(ctx context.Context, q querier) error {
		var err error
		if parent, err = attachParent(ctx, q, t.parentOID); err != nil {
			return err
		}
		if membership, err = readAttachMembership(ctx, q, t.bound); err != nil {
			return err
		}
		if membership.isPartitionOf(t.parentOID) {
			return nil
		}
		if err := membership.requireStandalone(t.bound); err != nil {
			return err
		}
		return requireAttachReady(ctx, q, t, nil)
	})
	if err != nil {
		return attachPartitionResult{}, err
	}
	if membership.isPartitionOf(t.parentOID) {
		// An earlier run's attach committed, or someone else attached the
		// table: the proofs are redundant either way.
		for _, p := range t.proofs() {
			if err := dropAttachProof(ctx, pool, p, holderWait, tracker); err != nil {
				return attachPartitionResult{}, err
			}
		}
		return attachPartitionResult{action: attachPartitionFound, bound: membership.bound}, nil
	}
	// INV: LK-2 — the ATTACH takes ACCESS EXCLUSIVE on the table and the
	// DEFAULT partition, so it requests its locks once no long-lived reader
	// or writer holds either, or once holderWait runs out.
	locked := make([]lockedTable, 0, 2)
	for _, p := range t.proofs() {
		locked = append(locked, p.lockedTables()...)
	}
	if _, err := waitOutsideLockQueue(ctx, pool, locked, holderWait, systemHolderWaitClock(), trackerHolderWaitReport(tracker)); err != nil {
		return attachPartitionResult{}, err
	}
	oids := map[uint32]uint32{}
	before := func(ctx context.Context, tx pgx.Tx) error {
		if err := lockAttachTables(ctx, tx, parent, t); err != nil {
			return err
		}
		// INV: ST-7 — the statement's names resolve, under its own
		// search_path, to the tables the run resolved, and the locks hold
		// them there until the transaction ends.
		if err := proveAttachNames(ctx, tx, t, parent); err != nil {
			return err
		}
		m, err := readAttachMembership(ctx, tx, t.bound)
		if err != nil {
			return err
		}
		if err := m.requireStandalone(t.bound); err != nil {
			return err
		}
		return requireAttachReady(ctx, tx, t, oids)
	}
	after := func(ctx context.Context, tx pgx.Tx) error {
		m, err := readAttachMembership(ctx, tx, t.bound)
		if err != nil {
			return err
		}
		if !m.isPartitionOf(t.parentOID) {
			return fmt.Errorf("%w: ST-7: %s is not a partition of %s after the attach",
				ErrInvariantViolation, t.bound.qualified(), qualifiedName(parent.schema, parent.name))
		}
		for _, p := range t.proofs() {
			if err := requireAttachProofState(ctx, tx, p, oids[p.tableOID], attachProofDone); err != nil {
				return err
			}
			reportAttachProofStatement(started, progress.OperationBrief, p.dropSQL())
			// INV: LK-2 — the DROP needs ACCESS EXCLUSIVE, which the ATTACH
			// already holds on this table, and changes only the catalog.
			if _, err := tx.Exec(ctx, p.dropSQL()); err != nil {
				return fmt.Errorf("drop attach proof %s on %s: %w", p.name(), p.qualified(), err)
			}
		}
		return nil
	}
	reportAttachProofStatement(started, progress.OperationBrief, t.st.SQL())
	if err := executeBoundedAttemptAs(ctx, pool, t.st, b, t.searchPath, "", before, after); err != nil {
		return attachPartitionResult{}, fmt.Errorf("attach %s to %s: %w", t.bound.qualified(), qualifiedName(parent.schema, parent.name), err)
	}
	var bound string
	err = tx.read(ctx, "read the bound of "+t.bound.qualified(), func(ctx context.Context, q querier) error {
		m, err := readAttachMembership(ctx, q, t.bound)
		bound = m.bound
		return err
	})
	if err != nil {
		return attachPartitionResult{}, err
	}
	return attachPartitionResult{action: attachPartitionAttached, bound: bound}, nil
}

// attachParent reads the parent and requires a partitioned table.
func attachParent(ctx context.Context, q querier, parentOID uint32) (relationFacts, error) {
	parent, found, err := relationByOID(ctx, q, parentOID)
	if err != nil {
		return relationFacts{}, err
	}
	if !found || parent.relkind != "p" {
		return relationFacts{}, fmt.Errorf("%w: ST-7: table %d is no longer the partitioned table the run resolved",
			ErrInvariantViolation, parentOID)
	}
	return parent, nil
}

// requireAttachReady requires the DEFAULT partition of the parent to be the
// one the run proved, or none, and every proof to be a validated CHECK. When
// oids is set, it records each proof's constraint OID by its table.
func requireAttachReady(ctx context.Context, q querier, t attachPartitionTarget, oids map[uint32]uint32) error {
	var defaultOID uint32
	err := q.QueryRow(ctx, attachDefaultPartitionSQL, t.parentOID).Scan(&defaultOID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: ST-7: table %d is no longer partitioned", ErrInvariantViolation, t.parentOID)
	}
	if err != nil {
		return fmt.Errorf("read the DEFAULT partition of table %d: %w", t.parentOID, err)
	}
	if defaultOID != t.deflt.tableOID {
		return fmt.Errorf("%w: the parent's DEFAULT partition is table %d, and the run proved table %d",
			errAttachPartitionNotReady, defaultOID, t.deflt.tableOID)
	}
	for _, p := range t.proofs() {
		state, oid, err := readAttachProof(ctx, q, p)
		if err != nil {
			return err
		}
		if state != attachProofDone {
			return fmt.Errorf("%w: %s has no validated CHECK %s", errAttachPartitionNotReady, p.qualified(), p.name())
		}
		if oids != nil {
			oids[p.tableOID] = oid
		}
	}
	return nil
}

// attachDefaultPartitionSQL reads the parent's DEFAULT partition, 0 for
// none. Every relation and operator is pg_catalog-qualified (CO-9).
const attachDefaultPartitionSQL = `SELECT p.partdefid
	FROM pg_catalog.pg_partitioned_table p
	WHERE p.partrelid OPERATOR(pg_catalog.=) $1`

// lockAttachTables takes, under the transaction's lock_timeout (LK-2), the
// locks the ATTACH takes, in its order: SHARE UPDATE EXCLUSIVE on the
// parent, then ACCESS EXCLUSIVE on its DEFAULT partition and on the table.
// It then proves each proof's table under its name, a plain table.
func lockAttachTables(ctx context.Context, tx pgx.Tx, parent relationFacts, t attachPartitionTarget) error {
	if err := lockOnly(ctx, tx, parent.schema, parent.name, "SHARE UPDATE EXCLUSIVE"); err != nil {
		return err
	}
	proofs := t.proofs()
	if t.hasDefault() {
		proofs = []attachProof{t.deflt, t.bound}
	}
	for _, p := range proofs {
		if err := lockOnly(ctx, tx, p.schema, p.table, "ACCESS EXCLUSIVE"); err != nil {
			return err
		}
		if err := lockAndProveAttachProofTable(ctx, tx, p); err != nil {
			return err
		}
	}
	return nil
}

// lockOnly locks one table, not its partitions, in mode.
func lockOnly(ctx context.Context, tx pgx.Tx, schema, table, mode string) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{schema, table}.Sanitize()+" IN "+mode+" MODE")
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
		return fmt.Errorf("%w: ST-7: table %s no longer exists under that name; it was renamed or dropped since the run resolved it",
			ErrInvariantViolation, qualifiedName(schema, table))
	}
	return err
}

// proveAttachNames requires the parent and the table, as the statement
// spells them, to resolve under the transaction's search_path to the tables
// the run resolved.
func proveAttachNames(ctx context.Context, tx pgx.Tx, t attachPartitionTarget, parent relationFacts) error {
	attach, _, err := statement.AttachedPartition(t.st.SQL())
	if err != nil {
		return fmt.Errorf("parse the attach: %w", err)
	}
	for _, name := range []struct {
		schema, table string
		want          uint32
	}{
		{attach.ParentSchema, attach.Parent, parent.oid},
		{attach.Schema, attach.Name, t.bound.tableOID},
	} {
		spelled := pgx.Identifier{name.table}
		if name.schema != "" {
			spelled = pgx.Identifier{name.schema, name.table}
		}
		var got uint32
		if err := tx.QueryRow(ctx, attachResolveSQL, spelled.Sanitize()).Scan(&got); err != nil {
			return fmt.Errorf("resolve %s: %w", spelled.Sanitize(), err)
		}
		if got != name.want {
			return fmt.Errorf("%w: ST-7: the attach's name %s resolves to table %d, and the run resolved table %d",
				ErrInvariantViolation, spelled.Sanitize(), got, name.want)
		}
	}
	return nil
}

// attachResolveSQL resolves a name as a statement under the session's
// search_path would, 0 for none. The function and cast are
// pg_catalog-qualified (CO-9).
const attachResolveSQL = `SELECT COALESCE(pg_catalog.to_regclass($1)::pg_catalog.oid, 0)`

// attachMembership is what the table is a child of.
type attachMembership struct {
	isPartition bool
	// parents are the tables it inherits from, a partition's one parent
	// included.
	parents []uint32
	// detachPending is whether a DETACH PARTITION CONCURRENTLY of it has not
	// finished.
	detachPending bool
	// bound is its FOR VALUES clause as the server prints it, empty unless
	// it is a partition.
	bound string
}

// attachMembershipSQL reads what the table is a child of. Every relation,
// function, and operator is pg_catalog-qualified (CO-9).
const attachMembershipSQL = `SELECT c.relispartition,
		ARRAY(SELECT h.inhparent FROM pg_catalog.pg_inherits h
		       WHERE h.inhrelid OPERATOR(pg_catalog.=) c.oid ORDER BY h.inhparent),
		EXISTS (SELECT 1 FROM pg_catalog.pg_inherits h
		         WHERE h.inhrelid OPERATOR(pg_catalog.=) c.oid AND h.inhdetachpending),
		COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), '')
	FROM pg_catalog.pg_class c
	WHERE c.oid OPERATOR(pg_catalog.=) $1`

// readAttachMembership reads what the proof's table is a child of.
func readAttachMembership(ctx context.Context, q querier, p attachProof) (attachMembership, error) {
	var m attachMembership
	err := q.QueryRow(ctx, attachMembershipSQL, p.tableOID).Scan(&m.isPartition, &m.parents, &m.detachPending, &m.bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, fmt.Errorf("%w: ST-7: table %s (%d) no longer exists", ErrInvariantViolation, p.qualified(), p.tableOID)
	}
	if err != nil {
		return m, fmt.Errorf("read the parents of %s: %w", p.qualified(), err)
	}
	return m, nil
}

// isPartitionOf reports whether the table is a partition of the parent, with
// no detach of it pending.
func (m attachMembership) isPartitionOf(parentOID uint32) bool {
	return m.isPartition && !m.detachPending && len(m.parents) == 1 && m.parents[0] == parentOID
}

// requireStandalone requires the table to be a child of nothing.
func (m attachMembership) requireStandalone(p attachProof) error {
	if m.isPartition || len(m.parents) > 0 {
		return fmt.Errorf("%w: ST-7: %s is a partition or inheritance child of table %v, not the standalone table the run resolved",
			ErrInvariantViolation, p.qualified(), m.parents)
	}
	return nil
}
