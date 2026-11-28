package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrDroppedIndexDrift means the index a reviewed plan drops is no longer
// the plain, valid index of the plan's table with the definition the plan
// carried: it is gone, belongs to another table, is invalid, is a
// partitioned table's index or an index partition, now backs a constraint,
// or is another index that took over its name. It is returned only from the
// read before the drop, so nothing was dropped. The outcome is permanent:
// re-plan and review the new plan.
var ErrDroppedIndexDrift = errors.New("the index the plan drops changed after planning")

// ErrDroppedIndexReplaced means a drop that passed its pre-drop read dropped
// an index the plan did not review: another session gave the name to a
// different index in the round trip between the read and the drop, and the
// index that was read is still in the catalog. Unlike ErrDroppedIndexDrift,
// something was dropped; the operator inspects the table's indexes and
// recreates the one that was dropped if it should stay. The outcome is
// permanent.
var ErrDroppedIndexReplaced = errors.New("the drop removed an index the plan did not review")

// ErrReplacementIndexDrift means the index that replaces the index or
// unique constraint a reviewed plan drops is no longer the valid index of the plan's table with the
// definition the plan built it with: it is gone, invalid, belongs to another
// table, or was rebuilt with another definition, as can happen while an
// apply is stopped between the build and the drop. It is returned only from
// the read before the drop, so the old index or constraint is still in
// place. The outcome
// is permanent: re-plan, which builds the replacement again.
var ErrReplacementIndexDrift = errors.New("the index that replaces the dropped one changed after planning")

// DropOutcomeUnknownError means DROP INDEX CONCURRENTLY returned success,
// so an index was dropped, but the lookup that confirms it was the reviewed
// one failed: the connection dropped or the caller's context ended right
// after the drop. Whether the reviewed index or one that took its name in
// between was dropped is unknown, so a caller must inspect the table's
// indexes before retrying. A rerun alone does not settle it: once the name
// is gone, it reports dropped-index-drift whichever index was dropped. Err
// keeps the cause for errors.Is and errors.As.
type DropOutcomeUnknownError struct {
	Schema, Table, Index string
	// IndexOID is the OID of the index the pre-drop read checked. If it is
	// no longer in the catalog, the reviewed index is the one dropped.
	IndexOID uint32
	Err      error
}

func (e *DropOutcomeUnknownError) Error() string {
	return fmt.Sprintf("DROP INDEX CONCURRENTLY %s completed, but the check that it dropped the reviewed index (oid %d) failed, "+
		"so which index was dropped is unknown until the indexes of %s are inspected: %v",
		pgx.Identifier{e.Schema, e.Index}.Sanitize(), e.IndexOID, pgx.Identifier{e.Schema, e.Table}.Sanitize(), e.Err)
}

func (e *DropOutcomeUnknownError) Unwrap() error { return e.Err }

// ExecuteDropIndexConcurrently drops one reviewed index with DROP INDEX
// CONCURRENTLY, so the drop never takes the ACCESS EXCLUSIVE lock a plain
// DROP INDEX holds while it waits behind the table's other transactions. A
// caller has already admitted the index as destructive; definition is the
// one the plan carried (schemadiff.DroppedObject.Definition).
//
// DROP INDEX CONCURRENTLY cannot run inside a transaction block, so the
// index is checked and dropped on one pooled session under the CONCURRENTLY
// wait policy (see ConcurrentBudget). Immediately before the drop the check
// refuses, dropping nothing, unless schema.index is still a valid plain
// index of schema.table that has definition and that the server will drop
// concurrently (ErrDroppedIndexDrift), and is not the replica identity
// index of a table, whose loss makes UPDATE and DELETE fail while a
// publication publishes them (ErrReplicaIdentityDrop).
//
// PostgreSQL drops an index only by name, so another session can swap the
// name in the round trip between the check and the drop. After the drop the
// checked index's OID is looked up once more. That lookup is detective
// only: it catches a swap that renamed the checked index aside, reports it
// as ErrDroppedIndexReplaced, and cannot undo the drop or see a swap that
// dropped the checked index as well. If the lookup itself fails, the drop
// returns a *DropOutcomeUnknownError.
//
// statement_timeout is the overall budget, or the caller's cancellation in
// caller-owned mode, and lock_timeout is disabled for the reasons
// ConcurrentBudget gives. A drop that does not finish returns a
// *BudgetError or the caller's cancellation and leaves the index as it was
// or marks it invalid; an invalid leftover is debris the diff reports as an
// advisory, never as a drop. It needs one pool connection.
func ExecuteDropIndexConcurrently(ctx context.Context, pool *pgxpool.Pool, schema, table, index, definition string, b ConcurrentBudget) (DroppedIndex, error) {
	return executeDropIndexConcurrently(ctx, pool, schema, table, index, definition, nil, b, progress.WallClock{})
}

// ExecuteReplacedIndexDropConcurrently is ExecuteDropIndexConcurrently for
// the old index of a changed index the plan replaces
// (schemadiff.Change.Replacement). On the same session, immediately before
// the drop, it also refuses, dropping nothing, unless the replacement is
// still a valid index of schema.table with the definition the plan built it
// with (ErrReplacementIndexDrift). The table then has the desired index
// before the old one goes. Like the check of the dropped index, this read
// cannot hold off a session that changes the replacement in the round trip
// before the drop; it catches a replacement that changed while the apply
// was not running.
func ExecuteReplacedIndexDropConcurrently(ctx context.Context, pool *pgxpool.Pool, schema, table, index, definition string, replacement schemadiff.IndexReplacement, b ConcurrentBudget) (DroppedIndex, error) {
	if replacement.Index == "" || replacement.Def == "" {
		return DroppedIndex{}, fmt.Errorf("%w: dropping index %s.%s for its replacement needs the replacement's name and definition, got %q, %q",
			ErrInvariantViolation, schema, index, replacement.Index, replacement.Def)
	}
	return executeDropIndexConcurrently(ctx, pool, schema, table, index, definition, &replacement, b, progress.WallClock{})
}

// executeDropIndexConcurrently is ExecuteDropIndexConcurrently with the
// replacement to check, nil for an index the plan removes, and the clock it
// reads right before the drop and again right after it.
func executeDropIndexConcurrently(ctx context.Context, pool *pgxpool.Pool, schema, table, index, definition string, replacement *schemadiff.IndexReplacement, b ConcurrentBudget, clock progress.Clock) (DroppedIndex, error) {
	if err := b.validate(); err != nil {
		return DroppedIndex{}, err
	}
	// INV: LK-2 — in caller-owned mode the caller's cancellation is the
	// drop's only bound, so a context without one is refused before any
	// session use.
	if b.CallerOwned && ctx.Done() == nil {
		return DroppedIndex{}, ErrCallerOwnedNeedsCancellableContext
	}
	if schema == "" || table == "" || index == "" {
		return DroppedIndex{}, fmt.Errorf("%w: a drop needs the schema, table, and index names, got %q, %q, %q",
			ErrUnqualifiedTable, schema, table, index)
	}
	if definition == "" {
		return DroppedIndex{}, fmt.Errorf("%w: dropping index %s.%s needs the definition the plan carried for it",
			ErrInvariantViolation, schema, index)
	}
	ref := pgx.Identifier{schema, index}
	tableRef := pgx.Identifier{schema, table}

	conn, release, err := acquireBudgetedSession(ctx, pool, b, schema)
	if err != nil {
		return DroppedIndex{}, err
	}
	defer release()

	// The name must resolve to a valid, plain index of the plan's table
	// with the planned definition, read on the session that drops it,
	// immediately before the drop.
	var (
		oid         uint32
		valid       bool
		droppable   bool
		replicaHead bool
	)
	err = conn.QueryRow(ctx,
		`SELECT c.oid, i.indisvalid, `+droppableColumn+`,
		        (i.indisreplident AND t.relreplident OPERATOR(pg_catalog.=) 'i')
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		   JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		   JOIN pg_catalog.pg_namespace tn ON tn.oid OPERATOR(pg_catalog.=) t.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		    AND tn.nspname OPERATOR(pg_catalog.=) $1 AND t.relname OPERATOR(pg_catalog.=) $3`,
		schema, index, table).Scan(&oid, &valid, &droppable, &replicaHead)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return DroppedIndex{}, fmt.Errorf("%w: %s has no index %s on %s any more; re-plan",
			ErrDroppedIndexDrift, schema, ref.Sanitize(), tableRef.Sanitize())
	case err != nil:
		return DroppedIndex{}, fmt.Errorf("read index %s before dropping it: %w", ref.Sanitize(), err)
	}
	switch {
	case !valid:
		return DroppedIndex{}, fmt.Errorf("%w: %s is invalid, which is unfinished build debris and not what the plan reviewed; re-plan",
			ErrDroppedIndexDrift, ref.Sanitize())
	case !droppable:
		return DroppedIndex{}, fmt.Errorf("%w: %s is now a constraint's index, a partitioned table's index, or an index partition, "+
			"which DROP INDEX CONCURRENTLY does not drop; re-plan", ErrDroppedIndexDrift, ref.Sanitize())
	case replicaHead:
		return DroppedIndex{}, fmt.Errorf("%w: %s is the replica identity index of %s, which makes UPDATE and DELETE fail "+
			"while a publication publishes them; point REPLICA IDENTITY at another unique index, at a primary key (DEFAULT), "+
			"or at the whole row (FULL) first, then re-plan", ErrReplicaIdentityDrop, ref.Sanitize(), tableRef.Sanitize())
	}
	live, found, err := schemadiff.ReadDroppedObject(ctx, conn, schema, table, schemadiff.DroppedIndex, index)
	switch {
	case err != nil:
		return DroppedIndex{}, fmt.Errorf("read the definition of index %s before dropping it: %w", ref.Sanitize(), err)
	case !found:
		return DroppedIndex{}, fmt.Errorf("%w: %s has no index %s on %s any more; re-plan",
			ErrDroppedIndexDrift, schema, ref.Sanitize(), tableRef.Sanitize())
	case live.Definition != definition:
		return DroppedIndex{}, fmt.Errorf("%w: index %s is now %q, not the %q the plan reviewed; re-plan",
			ErrDroppedIndexDrift, ref.Sanitize(), live.Definition, definition)
	}
	// The definition was read by name in a statement of its own, so the name
	// must still resolve to the OID whose flags were checked: otherwise the
	// definition compared above may belong to an index that took the name in
	// between.
	var nowOID uint32
	err = conn.QueryRow(ctx,
		`SELECT c.oid FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
		schema, index).Scan(&nowOID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return DroppedIndex{}, fmt.Errorf("%w: %s has no index %s on %s any more; re-plan",
			ErrDroppedIndexDrift, schema, ref.Sanitize(), tableRef.Sanitize())
	case err != nil:
		return DroppedIndex{}, fmt.Errorf("confirm index %s before dropping it: %w", ref.Sanitize(), err)
	case nowOID != oid:
		return DroppedIndex{}, fmt.Errorf("%w: the name %s moved from oid %d to oid %d while it was checked; re-plan",
			ErrDroppedIndexDrift, ref.Sanitize(), oid, nowOID)
	}

	if replacement != nil {
		if err := refuseReplacementIndexDrift(ctx, conn, schema, table, *replacement); err != nil {
			return DroppedIndex{}, err
		}
	}

	start := clock.Now()
	_, dropErr := conn.Exec(ctx, "DROP INDEX CONCURRENTLY "+ref.Sanitize())
	elapsed := clock.Now().Sub(start)
	if dropErr != nil {
		return DroppedIndex{}, asConcurrentBudgetError(ctx, dropErr, b, elapsed, "drop index "+ref.Sanitize())
	}
	// PostgreSQL has no drop by OID, so this lookup is after the fact: it
	// cannot stop a drop of the wrong index, only report one.
	var remaining int
	if err := conn.QueryRow(ctx,
		`SELECT pg_catalog.count(*) FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) $1`,
		oid).Scan(&remaining); err != nil {
		return DroppedIndex{}, &DropOutcomeUnknownError{Schema: schema, Table: table, Index: index, IndexOID: oid, Err: err}
	}
	if remaining != 0 {
		return DroppedIndex{}, fmt.Errorf("%w: %s was dropped by name, but the index the plan checked (oid %d) is still in the catalog, "+
			"so another session gave the name to a different index between the check and the drop and that index is the one that was dropped; "+
			"inspect the indexes of %s and recreate the dropped one if it should stay",
			ErrDroppedIndexReplaced, ref.Sanitize(), oid, tableRef.Sanitize())
	}
	return DroppedIndex{Schema: schema, Index: index, IndexOID: oid, Duration: elapsed}, nil
}

// refuseReplacementIndexDrift refuses unless r.Index is a valid index of
// schema.table whose definition, read the way introspection reads it, is
// r.Def. Both are printed under r.Index by the same deparser, so the
// comparison sees the definition the plan built.
func refuseReplacementIndexDrift(ctx context.Context, q querier, schema, table string, r schemadiff.IndexReplacement) error {
	ref := pgx.Identifier{schema, r.Index}
	tableRef := pgx.Identifier{schema, table}
	var (
		def   string
		valid bool
	)
	err := q.QueryRow(ctx,
		`SELECT pg_catalog.pg_get_indexdef(i.indexrelid), i.indisvalid
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		   JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		   JOIN pg_catalog.pg_namespace tn ON tn.oid OPERATOR(pg_catalog.=) t.relnamespace
		  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		    AND tn.nspname OPERATOR(pg_catalog.=) $1 AND t.relname OPERATOR(pg_catalog.=) $3`,
		schema, r.Index, table).Scan(&def, &valid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s has no index %s on %s any more, so dropping the index or constraint it replaces would leave the table without its new definition; re-plan",
			ErrReplacementIndexDrift, schema, ref.Sanitize(), tableRef.Sanitize())
	case err != nil:
		return fmt.Errorf("read replacement index %s before dropping the index it replaces: %w", ref.Sanitize(), err)
	case !valid:
		return fmt.Errorf("%w: replacement index %s is invalid, so dropping the index or constraint it replaces would leave the table without its new definition; re-plan",
			ErrReplacementIndexDrift, ref.Sanitize())
	}
	unqualified, err := statement.Qualify(def, "")
	if err != nil {
		return fmt.Errorf("unqualify replacement index %s: %w", ref.Sanitize(), err)
	}
	got, err := statement.RenameIndex(unqualified, r.Index)
	if err != nil {
		return fmt.Errorf("read the definition of replacement index %s: %w", ref.Sanitize(), err)
	}
	if got != r.Def {
		return fmt.Errorf("%w: replacement index %s is now %q, not the %q the plan built; re-plan",
			ErrReplacementIndexDrift, ref.Sanitize(), got, r.Def)
	}
	return nil
}
