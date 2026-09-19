package preflight

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CopySwapRefusalCause identifies which table shape put a target outside the
// copy-and-swap route's v1 scope. The zero value means the shape is supported.
type CopySwapRefusalCause string

const (
	// CopySwapCausePKUnsupported means the table has no primary key, a
	// multi-column one, or one whose type is outside the integer family
	// the chunker keys on.
	CopySwapCausePKUnsupported CopySwapRefusalCause = "copy-and-swap-pk-unsupported"
	// CopySwapCauseReplicaIdentity means the table's replica identity is
	// neither DEFAULT nor FULL, so decoded UPDATE and DELETE events would
	// not carry the key the applier needs.
	CopySwapCauseReplicaIdentity CopySwapRefusalCause = "copy-and-swap-replica-identity"
	// CopySwapCauseForeignKeys means a foreign key references the table or
	// leaves it; either is bound to the table's OID and would follow the
	// retained old table through a rename swap.
	CopySwapCauseForeignKeys CopySwapRefusalCause = "copy-and-swap-foreign-keys"
	// CopySwapCauseTriggers means the table carries a user trigger or a
	// rewrite rule, both OID-bound dependents a rename swap strands.
	CopySwapCauseTriggers CopySwapRefusalCause = "copy-and-swap-triggers"
	// CopySwapCausePartitioned means the table is a partitioned parent, a
	// partition, or part of an inheritance tree.
	CopySwapCausePartitioned CopySwapRefusalCause = "copy-and-swap-partitioned"
	// CopySwapCauseUnlogged means the table is UNLOGGED, while the shadow
	// created by LIKE would be permanent.
	CopySwapCauseUnlogged CopySwapRefusalCause = "copy-and-swap-unlogged"
	// CopySwapCauseForceRLS means the table has FORCE ROW LEVEL SECURITY:
	// the copier runs as the owner, whose reads of the source would be
	// filtered and whose writes into the shadow would be rejected by the
	// replicated policies, and the shadow must carry those policies before
	// it holds a row.
	CopySwapCauseForceRLS CopySwapRefusalCause = "copy-and-swap-force-rls"
)

// CopySwapRefusalCauses returns the closed set of copy-and-swap shape refusal
// causes, so documentation and consumers can enumerate them instead of
// maintaining their own list.
func CopySwapRefusalCauses() []CopySwapRefusalCause {
	return []CopySwapRefusalCause{
		CopySwapCausePKUnsupported,
		CopySwapCauseReplicaIdentity,
		CopySwapCauseForeignKeys,
		CopySwapCauseTriggers,
		CopySwapCausePartitioned,
		CopySwapCauseUnlogged,
		CopySwapCauseForceRLS,
	}
}

// UnsupportedCopySwapShapeError reports that the target table's shape is
// outside the copy-and-swap route's v1 scope. Detail names the catalog fact
// that decided it; identifiers in it appear unquoted, so a renderer
// embedding it in structured output owns escaping it.
type UnsupportedCopySwapShapeError struct {
	// Cause is the shape that triggered the refusal.
	Cause CopySwapRefusalCause
	// Detail is the catalog fact behind the cause.
	Detail string
}

// Error implements the error interface.
func (e *UnsupportedCopySwapShapeError) Error() string {
	return fmt.Sprintf("copy-and-swap refuses the table shape (%s): %s", e.Cause, e.Detail)
}

// ErrCopySwapProofMismatch is returned when the privilege proof handed to the
// shape check was not verified at the copy-and-swap tier.
var ErrCopySwapProofMismatch = errors.New("privilege proof was not verified at the copy-and-swap tier")

// copySwapShapeFacts is one catalog snapshot of every shape fact the
// copy-and-swap route decides on, gathered in a single round trip so the
// checks cannot disagree about when they looked.
type copySwapShapeFacts struct {
	schema          string
	oid             uint32
	relkind         string
	relpersistence  string
	forceRLS        bool
	isPartition     bool
	hasSubclass     bool
	inheritsParents int64
	replicaIdentity string
	pkColumns       int64
	pkColumn        string
	pkType          string
	foreignKeysOut  int64
	foreignKeysIn   int64
	triggers        int64
	rules           int64
}

// CheckCopySwapShape verifies that schema.table (search_path resolution when
// schema is empty) has the shape the copy-and-swap route supports in v1: an
// ordinary table outside any partition or inheritance tree, exactly one
// smallint, integer, or bigint primary-key column, a replica identity of
// DEFAULT or FULL, no foreign keys, triggers, or rules, and no FORCE ROW
// LEVEL SECURITY. The role proof
// must have been verified at TierCopyAndSwap; the owner it carries is the
// role the shadow builder creates shadow objects as. On success it returns
// the CopySwapTarget proof carrying the catalog-resolved schema.
func CheckCopySwapShape(ctx context.Context, pool *pgxpool.Pool, schema, table string, role PrivilegedRole) (CopySwapTarget, error) {
	if role.Tier() != TierCopyAndSwap || role.Owner() == "" {
		return CopySwapTarget{}, fmt.Errorf("%w: tier %q, owner %q", ErrCopySwapProofMismatch, role.Tier(), role.Owner())
	}
	facts, err := gatherCopySwapShapeFacts(ctx, pool, schema, table)
	if err != nil {
		return CopySwapTarget{}, err
	}
	if cause, detail := refuseCopySwapShape(facts); cause != "" {
		return CopySwapTarget{}, &UnsupportedCopySwapShapeError{Cause: cause, Detail: detail}
	}
	// INV: ST-6, RF-1, RF-2, RF-3
	return CopySwapTarget{
		schema:    facts.schema,
		table:     table,
		pkColumn:  facts.pkColumn,
		pkType:    PKType(facts.pkType),
		ownerRole: role.Owner(),
		oid:       facts.oid,
	}, nil
}

// RecheckCopySwapShape verifies inside the build transaction that the proven
// relation still has the admitted shape and identity.
func RecheckCopySwapShape(ctx context.Context, tx pgx.Tx, target CopySwapTarget) error {
	facts, err := gatherCopySwapShapeFacts(ctx, tx, target.schema, target.table)
	if err != nil {
		return err
	}
	if facts.oid != target.oid {
		return fmt.Errorf("%w: relation OID changed from %d to %d", ErrCopySwapProofMismatch, target.oid, facts.oid)
	}
	if cause, detail := refuseCopySwapShape(facts); cause != "" {
		return &UnsupportedCopySwapShapeError{Cause: cause, Detail: detail}
	}
	return nil
}

// refuseCopySwapShape decides the first cause that puts the facts outside
// v1 scope, or the zero cause when the shape is supported. Whole-table
// facts are decided before key and dependent facts, so a table with several
// disqualifying shapes reports the one that needs the largest change.
func refuseCopySwapShape(f copySwapShapeFacts) (CopySwapRefusalCause, string) {
	if f.relpersistence == "u" {
		return CopySwapCauseUnlogged, "the table is UNLOGGED; the copy-and-swap shadow must be permanent"
	}
	if f.forceRLS {
		return CopySwapCauseForceRLS, "the table has FORCE ROW LEVEL SECURITY; the owner-run copier would be subject to its policies"
	}
	switch {
	case f.relkind == "p":
		return CopySwapCausePartitioned, "the table is a partitioned parent"
	case f.isPartition:
		return CopySwapCausePartitioned, "the table is a partition"
	case f.inheritsParents > 0 || f.hasSubclass:
		return CopySwapCausePartitioned, "the table is part of an inheritance tree"
	}
	if f.pkColumns != 1 {
		return CopySwapCausePKUnsupported, fmt.Sprintf("the primary key has %d columns; exactly one is required", f.pkColumns)
	}
	switch PKType(f.pkType) {
	case PKSmallint, PKInteger, PKBigint:
	default:
		return CopySwapCausePKUnsupported, fmt.Sprintf("primary-key column %s has type %s; smallint, integer, or bigint is required", f.pkColumn, f.pkType)
	}
	// pg_class.relreplident: d = DEFAULT (the primary key), f = FULL,
	// n = NOTHING, i = a named index.
	switch f.replicaIdentity {
	case "d", "f":
	case "n":
		return CopySwapCauseReplicaIdentity, "replica identity is NOTHING; DEFAULT or FULL is required"
	default:
		return CopySwapCauseReplicaIdentity, "replica identity is a named index; DEFAULT or FULL is required"
	}
	if f.foreignKeysIn > 0 || f.foreignKeysOut > 0 {
		return CopySwapCauseForeignKeys, fmt.Sprintf("%d foreign keys reference the table and %d leave it", f.foreignKeysIn, f.foreignKeysOut)
	}
	if f.triggers > 0 || f.rules > 0 {
		return CopySwapCauseTriggers, fmt.Sprintf("the table has %d user triggers and %d rules", f.triggers, f.rules)
	}
	return "", ""
}

// gatherCopySwapShapeFacts snapshots the shape facts in one query. The
// primary-key facts come from the table's PRIMARY KEY constraint; a table
// without one reports zero key columns. Internal triggers (the ones a
// foreign key installs) are excluded from the trigger count because the
// foreign-key cause already accounts for them.
func gatherCopySwapShapeFacts(ctx context.Context, db rowQuerier, schema, table string) (copySwapShapeFacts, error) {
	const q = `
		SELECT n.nspname::text, c.oid,
		       c.relkind::text,
		       c.relpersistence::text,
		       c.relforcerowsecurity,
		       c.relispartition,
		       c.relhassubclass,
		       (SELECT count(*) FROM pg_inherits i WHERE i.inhrelid = c.oid),
		       c.relreplident::text,
		       COALESCE((SELECT cardinality(k.conkey) FROM pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'p'), 0),
		       COALESCE((SELECT a.attname::text FROM pg_constraint k JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.conkey[1]
		                 WHERE k.conrelid = c.oid AND k.contype = 'p' AND cardinality(k.conkey) = 1), ''),
		       COALESCE((SELECT format_type(a.atttypid, a.atttypmod) FROM pg_constraint k JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.conkey[1]
		                 WHERE k.conrelid = c.oid AND k.contype = 'p' AND cardinality(k.conkey) = 1), ''),
		       (SELECT count(*) FROM pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'f'),
		       (SELECT count(*) FROM pg_constraint k WHERE k.confrelid = c.oid AND k.contype = 'f'),
		       (SELECT count(*) FROM pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal),
		       (SELECT count(*) FROM pg_rewrite r WHERE r.ev_class = c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass(
			CASE WHEN $1 = '' THEN quote_ident($2)
			     ELSE quote_ident($1) || '.' || quote_ident($2) END)`
	var f copySwapShapeFacts
	err := db.QueryRow(ctx, q, schema, table).Scan(
		&f.schema, &f.oid, &f.relkind, &f.relpersistence, &f.forceRLS, &f.isPartition, &f.hasSubclass, &f.inheritsParents, &f.replicaIdentity,
		&f.pkColumns, &f.pkColumn, &f.pkType, &f.foreignKeysOut, &f.foreignKeysIn, &f.triggers, &f.rules)
	if errors.Is(err, pgx.ErrNoRows) {
		return copySwapShapeFacts{}, unresolvedTargetCause(ctx, db, schema, table)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateInsufficientPrivilege {
		return copySwapShapeFacts{}, unresolvedTargetCause(ctx, db, schema, table)
	}
	if err != nil {
		return copySwapShapeFacts{}, fmt.Errorf("gather copy-and-swap shape facts for %s: %w", qualifiedName(schema, table), err)
	}
	if f.relkind != "r" && f.relkind != "p" {
		return copySwapShapeFacts{}, fmt.Errorf("%w: %s has relkind %q", ErrNotTable, qualifiedName(schema, table), f.relkind)
	}
	return f, nil
}
