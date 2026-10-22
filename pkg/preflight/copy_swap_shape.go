package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/refusal"
)

// EngineSchema is the schema the engine owns in every target database: the
// checkpoint table lives in it, and so does every function the engine
// installs for a run. A trigger on the target whose function lives in this
// schema is the engine's own, not a user trigger, and the shape check sets
// it aside the way it sets aside the engine's own publication.
const EngineSchema = "pgsprite"

// CopySwapRefusalCause identifies which table shape put a target outside the
// copy-and-swap route's v1 scope. The zero value means the shape is supported.
type CopySwapRefusalCause string

const (
	// CopySwapCausePKUnsupported means the table has no primary key, a
	// multi-column one, one whose type is outside the integer family the
	// chunker keys on, or a DEFERRABLE one. A deferrable key is checked at
	// the end of each statement rather than per row, so one statement can
	// move a row onto a key another row still holds and the decoded stream
	// no longer has one row per key, which the applier's buffer relies on;
	// the server also declines to use it as the DEFAULT replica identity,
	// so every UPDATE on a published table would fail.
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
	// CopySwapCauseDependentViews means a view or materialized view
	// selects from the table; its rewrite rule is bound to the table's OID
	// and would keep reading the retained old table after the swap.
	CopySwapCauseDependentViews CopySwapRefusalCause = "copy-and-swap-dependent-views"
	// CopySwapCausePublicationMember means a publication other than the
	// engine's own publishes the table. Explicit membership is bound to the
	// table's OID, so subscribers would keep following the old table; a
	// FOR ALL TABLES or FOR TABLES IN SCHEMA publication publishes every
	// new table in its scope, the shadow included, so its subscribers would
	// receive the copy's writes to a relation they do not have.
	CopySwapCausePublicationMember CopySwapRefusalCause = "copy-and-swap-publication-member"
	// CopySwapCauseSubscriptionTarget means a subscription on this database
	// applies changes into the table; the subscription binds it by OID, so
	// after the swap the apply worker would find no state for the new
	// relation and skip its changes without an error.
	CopySwapCauseSubscriptionTarget CopySwapRefusalCause = "copy-and-swap-subscription-target"
	// CopySwapCauseDependents means an object the causes above do not name
	// depends on the table's OID or its row type — a rule on another table
	// that writes into it, a SQL-standard function body that reads it, a
	// policy on another table whose expression consults it, a column of its
	// row type — and the swap carries none of them to the new table.
	CopySwapCauseDependents CopySwapRefusalCause = "copy-and-swap-dependents"
)

// CopySwapRefusalCauses returns the closed set of copy-and-swap refusal
// causes — the shape causes above and the environment causes in
// copy_swap_environment.go — so documentation and consumers can enumerate
// them instead of maintaining their own list.
func CopySwapRefusalCauses() []CopySwapRefusalCause {
	return []CopySwapRefusalCause{
		CopySwapCausePKUnsupported,
		CopySwapCauseReplicaIdentity,
		CopySwapCauseForeignKeys,
		CopySwapCauseTriggers,
		CopySwapCausePartitioned,
		CopySwapCauseUnlogged,
		CopySwapCauseForceRLS,
		CopySwapCauseDependentViews,
		CopySwapCausePublicationMember,
		CopySwapCauseSubscriptionTarget,
		CopySwapCauseDependents,
		CopySwapCauseLogicalDecodingUnavailable,
		CopySwapCauseSlotCollision,
		CopySwapCauseSlotHeadroom,
		CopySwapCauseDiskHeadroom,
	}
}

// Class is the routing class of the cause, from docs/refusal-classes.md:
// what kind of boundary the consumer is looking at. A replica identity the
// operator can set, and every cluster and volume cause — a server setting,
// a foreign slot, exhausted slot or sender headroom, a short or unmeasured
// volume — are environmental: the same table is admitted once state outside
// the engine changes, and no engine release is waited on. Every other shape
// cause is a capability boundary: a wider key family, an OID-bound dependent
// re-pointed at cutover, a persistence or row-security path the engine has
// not built. The class is a triage decision; the cause's row in the document
// decides what the consumer does next. The zero cause, and an unknown one,
// have no class.
func (c CopySwapRefusalCause) Class() refusal.Class {
	switch c {
	case CopySwapCauseReplicaIdentity,
		CopySwapCauseLogicalDecodingUnavailable,
		CopySwapCauseSlotCollision,
		CopySwapCauseSlotHeadroom,
		CopySwapCauseDiskHeadroom:
		return refusal.ClassEnvironmental
	case CopySwapCausePKUnsupported,
		CopySwapCauseForeignKeys,
		CopySwapCauseTriggers,
		CopySwapCausePartitioned,
		CopySwapCauseUnlogged,
		CopySwapCauseForceRLS,
		CopySwapCauseDependentViews,
		CopySwapCausePublicationMember,
		CopySwapCauseSubscriptionTarget,
		CopySwapCauseDependents:
		return refusal.ClassCapabilityBoundary
	default:
		return ""
	}
}

// CopySwapRefusalCauseOf returns the cause carried by a copy-and-swap shape
// or environment refusal anywhere in err's chain, or the zero cause when
// err is neither.
func CopySwapRefusalCauseOf(err error) CopySwapRefusalCause {
	var shape *UnsupportedCopySwapShapeError
	if errors.As(err, &shape) {
		return shape.Cause
	}
	var env *CopySwapEnvironmentError
	if errors.As(err, &env) {
		return env.Cause
	}
	return ""
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
	database        string
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
	pkDeferrable    bool
	foreignKeysOut  int64
	foreignKeysIn   int64
	triggers        int64
	rules           int64
	dependentViews  int64
	// publications names every publication that publishes the table, in
	// any form, other than the engine's own for this table.
	publications  []string
	subscriptions int64
	// dependents describes every object that depends on the table's OID or
	// row type without being the table's own.
	dependents []string
}

// CheckCopySwapShape verifies that schema.table (search_path resolution when
// schema is empty) has the shape the copy-and-swap route supports in v1: an
// ordinary table outside any partition or inheritance tree, exactly one
// non-deferrable smallint, integer, or bigint primary-key column, a replica
// identity of DEFAULT or FULL, no FORCE ROW LEVEL SECURITY, no foreign keys, triggers,
// or rules, no dependent views, no publication other than the engine's own
// publishing it, no subscription applying into it, and no other object
// depending on its OID or row type. The role proof must have been verified
// at TierCopyAndSwap; the owner it carries is the role the shadow builder
// creates shadow objects as. On success it returns the CopySwapShape proof
// carrying the catalog-resolved database and schema, which only
// CheckCopySwapEnvironment accepts: the facts the server cannot settle from
// the table alone — logical decoding, slot and disk headroom — are that
// check's, and the CopySwapTarget every writing step requires is minted
// there. CheckCopySwap runs the whole sequence. The catalog reads are
// pg_catalog-qualified, so the result does not depend on the pool's
// search_path; the pool should still come from dbconn.NewPool, which bounds
// every session's timeouts.
func CheckCopySwapShape(ctx context.Context, pool *pgxpool.Pool, schema, table string, role PrivilegedRole) (CopySwapShape, error) {
	if role.Tier() != TierCopyAndSwap || role.Owner() == "" {
		return CopySwapShape{}, fmt.Errorf("%w: tier %q, owner %q", ErrCopySwapProofMismatch, role.Tier(), role.Owner())
	}
	facts, err := gatherCopySwapShapeFacts(ctx, pool, schema, table)
	if err != nil {
		return CopySwapShape{}, err
	}
	if cause, detail := refuseCopySwapShape(facts); cause != "" {
		return CopySwapShape{}, &UnsupportedCopySwapShapeError{Cause: cause, Detail: detail}
	}
	// INV: ST-6, RF-1, RF-2, RF-3
	return CopySwapShape{
		copySwapShape: copySwapShape{
			database:  facts.database,
			schema:    facts.schema,
			table:     table,
			pkColumn:  facts.pkColumn,
			pkType:    PKType(facts.pkType),
			ownerRole: role.Owner(),
			oid:       facts.oid,
		},
		logicalDecoding: role.LogicalDecoding(),
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
	if f.pkDeferrable {
		return CopySwapCausePKUnsupported, "the primary key is DEFERRABLE; a non-deferrable key is required"
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
	if f.dependentViews > 0 {
		return CopySwapCauseDependentViews, fmt.Sprintf("%d views or materialized views select from the table", f.dependentViews)
	}
	if len(f.publications) > 0 {
		return CopySwapCausePublicationMember, fmt.Sprintf("%d publications other than the engine's own publish the table: %s", len(f.publications), strings.Join(f.publications, ", "))
	}
	if f.subscriptions > 0 {
		return CopySwapCauseSubscriptionTarget, fmt.Sprintf("%d subscriptions apply changes into the table", f.subscriptions)
	}
	if len(f.dependents) > 0 {
		return CopySwapCauseDependents, fmt.Sprintf("%d objects depend on the table or its row type and would follow the retained old table: %s", len(f.dependents), strings.Join(f.dependents, "; "))
	}
	return "", ""
}

// gatherCopySwapShapeFacts snapshots the shape facts in one query. The
// primary-key facts come from the table's PRIMARY KEY constraint; a table
// without one reports zero key columns. Internal triggers (the ones a
// foreign key installs) are excluded from the trigger count because the
// foreign-key cause already accounts for them, and so is a trigger whose
// function lives in EngineSchema: that is the engine's own, installed for a
// run and removed with the retained old table, and a run that re-proves its
// table while it is in place must not refuse the table on it. Dependent views are the
// distinct views and materialized views whose rewrite rules pg_depend
// records against the table; the table's own rules are counted as rules.
// Publications are read in a second statement straight from the catalog —
// FOR ALL TABLES publications, FOR TABLE memberships, and FOR TABLES IN
// SCHEMA memberships of the table's schema — rather than through
// pg_publication_tables, whose set-returning function looks each
// publication it has listed up again by name and fails the whole read when
// another session has dropped one in between, as a concurrent run's drop of
// its own publication does; the engine's own publication for this table is
// set aside by its exact derived name. Dependents are the distinct pg_depend normal
// dependents of the table's OID or its row type, less the objects that are
// the table's own (an auto or internal edge back to it: indexes,
// constraints, owned sequences, triggers, policies, defaults); foreign
// keys and dependent views are among them, and their own causes decide
// first. Each is described the way pg_describe_object renders it. Every
// catalog name is pg_catalog-qualified so the facts resolve to the real
// catalog whatever search_path the session carries.
func gatherCopySwapShapeFacts(ctx context.Context, db rowQuerier, schema, table string) (copySwapShapeFacts, error) {
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, operators included (IN and IS DISTINCT FROM
	// resolve = through the search_path, so they are spelled out).
	const q = `
		SELECT pg_catalog.current_database()::pg_catalog.text, n.nspname::pg_catalog.text, c.oid,
		       c.relkind::pg_catalog.text,
		       c.relpersistence::pg_catalog.text,
		       c.relforcerowsecurity,
		       c.relispartition,
		       c.relhassubclass,
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_inherits i WHERE i.inhrelid OPERATOR(pg_catalog.=) c.oid),
		       c.relreplident::pg_catalog.text,
		       COALESCE((SELECT pg_catalog.cardinality(k.conkey) FROM pg_catalog.pg_constraint k WHERE k.conrelid OPERATOR(pg_catalog.=) c.oid AND k.contype OPERATOR(pg_catalog.=) 'p'), 0),
		       COALESCE((SELECT a.attname::pg_catalog.text FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) c.oid AND a.attnum OPERATOR(pg_catalog.=) k.conkey[1]
		                 WHERE k.conrelid OPERATOR(pg_catalog.=) c.oid AND k.contype OPERATOR(pg_catalog.=) 'p' AND pg_catalog.cardinality(k.conkey) OPERATOR(pg_catalog.=) 1), ''),
		       COALESCE((SELECT pg_catalog.format_type(a.atttypid, a.atttypmod) FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) c.oid AND a.attnum OPERATOR(pg_catalog.=) k.conkey[1]
		                 WHERE k.conrelid OPERATOR(pg_catalog.=) c.oid AND k.contype OPERATOR(pg_catalog.=) 'p' AND pg_catalog.cardinality(k.conkey) OPERATOR(pg_catalog.=) 1), ''),
		       COALESCE((SELECT k.condeferrable FROM pg_catalog.pg_constraint k WHERE k.conrelid OPERATOR(pg_catalog.=) c.oid AND k.contype OPERATOR(pg_catalog.=) 'p'), false),
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_constraint k WHERE k.conrelid OPERATOR(pg_catalog.=) c.oid AND k.contype OPERATOR(pg_catalog.=) 'f'),
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_constraint k WHERE k.confrelid OPERATOR(pg_catalog.=) c.oid AND k.contype OPERATOR(pg_catalog.=) 'f'),
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_trigger t
		         WHERE t.tgrelid OPERATOR(pg_catalog.=) c.oid
		           AND NOT t.tgisinternal
		           AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_proc p
		                             JOIN pg_catalog.pg_namespace pn ON pn.oid OPERATOR(pg_catalog.=) p.pronamespace
		                            WHERE p.oid OPERATOR(pg_catalog.=) t.tgfoid AND pn.nspname OPERATOR(pg_catalog.=) $3)),
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_rewrite r WHERE r.ev_class OPERATOR(pg_catalog.=) c.oid),
		       (SELECT pg_catalog.count(DISTINCT v.oid)
		          FROM pg_catalog.pg_depend d
		          JOIN pg_catalog.pg_rewrite r ON r.oid OPERATOR(pg_catalog.=) d.objid
		          JOIN pg_catalog.pg_class v ON v.oid OPERATOR(pg_catalog.=) r.ev_class
		         WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass
		           AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		           AND d.refobjid OPERATOR(pg_catalog.=) c.oid
		           AND v.oid OPERATOR(pg_catalog.<>) c.oid
		           AND (v.relkind OPERATOR(pg_catalog.=) 'v' OR v.relkind OPERATOR(pg_catalog.=) 'm')),
		       c.relnamespace,
		       pg_catalog.current_setting('server_version_num')::int,
		       (SELECT pg_catalog.count(*) FROM pg_catalog.pg_subscription_rel sr WHERE sr.srrelid OPERATOR(pg_catalog.=) c.oid),
		       (SELECT COALESCE(pg_catalog.array_agg(dep.description ORDER BY dep.description), '{}')
		          FROM (SELECT DISTINCT pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid) AS description
		                  FROM pg_catalog.pg_depend d
		                 WHERE d.deptype OPERATOR(pg_catalog.=) 'n'
		                   AND ((d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refobjid OPERATOR(pg_catalog.=) c.oid)
		                     OR (d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_type'::pg_catalog.regclass AND d.refobjid OPERATOR(pg_catalog.=) c.reltype))
		                   AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend own
		                                    WHERE own.classid OPERATOR(pg_catalog.=) d.classid AND own.objid OPERATOR(pg_catalog.=) d.objid
		                                      AND own.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass AND own.refobjid OPERATOR(pg_catalog.=) c.oid
		                                      AND (own.deptype OPERATOR(pg_catalog.=) 'a' OR own.deptype OPERATOR(pg_catalog.=) 'i'))) dep)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(
			CASE WHEN $1 OPERATOR(pg_catalog.=) '' THEN pg_catalog.quote_ident($2)
			     ELSE pg_catalog.quote_ident($1) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident($2) END)`
	var f copySwapShapeFacts
	var namespaceOID uint32
	var version int
	err := db.QueryRow(ctx, q, schema, table, EngineSchema).Scan(
		&f.database, &f.schema, &f.oid, &f.relkind, &f.relpersistence, &f.forceRLS, &f.isPartition, &f.hasSubclass, &f.inheritsParents, &f.replicaIdentity,
		&f.pkColumns, &f.pkColumn, &f.pkType, &f.pkDeferrable, &f.foreignKeysOut, &f.foreignKeysIn, &f.triggers, &f.rules, &f.dependentViews,
		&namespaceOID, &version, &f.subscriptions, &f.dependents)
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
	publications, err := publicationsOf(ctx, db, f.oid, namespaceOID, version)
	if err != nil {
		return copySwapShapeFacts{}, fmt.Errorf("gather copy-and-swap shape facts for %s: %w", qualifiedName(schema, table), err)
	}
	f.publications = publicationsOtherThanEngine(publications, CopySwapDecodingName(f.database, f.schema, table))
	return f, nil
}

// schemaPublicationsSince is the server_version_num from which a
// publication can publish a whole schema (FOR TABLES IN SCHEMA), and from
// which the catalog that records those memberships exists.
const schemaPublicationsSince = 150000

// publicationsOf names every publication that publishes the table with
// the given OID, in the schema with the given OID, reading the catalogs
// that record membership directly: pg_publication for FOR ALL TABLES,
// pg_publication_rel for a FOR TABLE membership, and, on a server that
// has it, pg_publication_namespace for a FOR TABLES IN SCHEMA membership.
// The caller has already settled that the relation is a plain table, so
// the publications a partition would inherit from a published parent do
// not arise.
func publicationsOf(ctx context.Context, db rowQuerier, tableOID, namespaceOID uint32, version int) ([]string, error) {
	inSchema := `false`
	args := []any{tableOID}
	if version >= schemaPublicationsSince {
		inSchema = `EXISTS (SELECT 1 FROM pg_catalog.pg_publication_namespace pn WHERE pn.pnpubid OPERATOR(pg_catalog.=) p.oid AND pn.pnnspid OPERATOR(pg_catalog.=) $2)`
		args = append(args, namespaceOID)
	}
	var publications []string
	// INV: CO-9 — pg_catalog-qualified like the shape facts it completes.
	err := db.QueryRow(ctx, `
		SELECT COALESCE(pg_catalog.array_agg(p.pubname::pg_catalog.text ORDER BY p.pubname), '{}')
		FROM pg_catalog.pg_publication p
		WHERE p.puballtables
		   OR EXISTS (SELECT 1 FROM pg_catalog.pg_publication_rel r WHERE r.prpubid OPERATOR(pg_catalog.=) p.oid AND r.prrelid OPERATOR(pg_catalog.=) $1)
		   OR `+inSchema, args...).Scan(&publications)
	if err != nil {
		return nil, fmt.Errorf("read publications: %w", err)
	}
	return publications, nil
}

// publicationsOtherThanEngine drops the engine's own publication for the
// table from the names that publish it; that one is the route's doing, not
// a dependent.
func publicationsOtherThanEngine(publications []string, engine string) []string {
	var others []string
	for _, name := range publications {
		if name != engine {
			others = append(others, name)
		}
	}
	return others
}
