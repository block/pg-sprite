package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sqlstateInsufficientPrivilege is raised when qualified-name resolution
// hits a schema the role lacks USAGE on. Postgres errors are matched by
// SQLSTATE, never by message text.
const sqlstateInsufficientPrivilege = "42501"

// Tier is the access level a schema change's plan requires from the engine
// role, per the tiered contract in docs/engine-role.md. Each tier includes
// everything below it; a change is admitted at the tier its plan requires
// and nothing higher.
type Tier int

// The contract's tiers, lowest to highest.
const (
	// TierConnect covers connecting and resolving the target: CONNECT on
	// the database and USAGE on the target schema. The CONNECT rung
	// documents the contract rather than catching live failures — a role
	// missing it fails at connection time, before any check runs — while
	// the USAGE rung is load-bearing: without it a qualified target
	// masquerades as "table not found".
	TierConnect Tier = iota
	// TierAlterInPlace covers owner-gated in-place ALTER TABLE (the
	// instant and fast native paths), ALTER INDEX ... RENAME TO,
	// ALTER TYPE ... ADD VALUE, and a reviewed DROP TABLE: inheritable
	// membership in the role that owns the table, or the type.
	TierAlterInPlace
	// TierIndexBuild covers CREATE INDEX [CONCURRENTLY]: CREATE on the
	// target schema on top of owning-role membership.
	TierIndexBuild
	// TierCopyAndSwap covers shadow-object creation: membership usable
	// with SET ROLE, so shadow objects are born with the correct owner.
	TierCopyAndSwap
	// TierCreateTable covers greenfield CREATE TABLE and sits off the
	// ladder above: a table that does not exist yet has no owner to be a
	// member of, so the create path proves CONNECT on the database plus
	// USAGE and CREATE on the schema — deliberately not the ownership
	// membership the ALTER tiers require. It is checked by
	// CheckCreatePrivileges, never by CheckPrivileges, whose ladder walks
	// facts about an existing table.
	TierCreateTable
)

// String names the tier's capability for refusal messages.
func (t Tier) String() string {
	switch t {
	case TierConnect:
		return "connect and resolve the target"
	case TierAlterInPlace:
		return "in-place changes that need ownership"
	case TierIndexBuild:
		return "index builds"
	case TierCopyAndSwap:
		return "copy-and-swap"
	case TierCreateTable:
		return "create a new table"
	default:
		return fmt.Sprintf("unknown tier %d", int(t))
	}
}

// Requirement states the access a schema change's plan needs: the tier, and
// whether the strategy decodes WAL (logical-decoding CDC, copy-and-swap
// only), which additionally requires replication access.
type Requirement struct {
	Tier Tier
	// LogicalDecoding requires replication access on top of the tier:
	// rds_replication membership where that role exists (Aurora/RDS), the
	// REPLICATION role attribute otherwise. Only valid with
	// TierCopyAndSwap — no other strategy decodes WAL.
	LogicalDecoding bool
}

// PrivilegeError reports a failed access check: the tier that needs it, the
// catalog check that returned false, and the exact statement that would
// satisfy it. It is a refusal input, not an operational failure — the same
// fail-closed posture as every other refusal in the engine. Provisioning
// rationale lives in docs/engine-role.md.
type PrivilegeError struct {
	// Tier is the access level the failed check belongs to.
	Tier Tier
	// Check is the catalog predicate that returned false. It is display
	// prose: identifiers appear unquoted, so a renderer embedding it in
	// structured output (a markdown table, a PR comment) owns escaping it.
	Check string
	// Grant is the exact statement that would satisfy the check. Every
	// identifier in it is Sanitize()-quoted, so it is safe to echo
	// verbatim as executable SQL.
	Grant string
	// Hint explains the remediation when the Grant alone would surprise
	// the operator — for example when its grantee differs from the role
	// the Check names. Empty when the Grant speaks for itself.
	Hint string
}

// Error implements the error interface.
func (e *PrivilegeError) Error() string {
	msg := fmt.Sprintf("engine role lacks access for %s: %s is false; provision with: %s",
		e.Tier, e.Check, e.Grant)
	if e.Hint != "" {
		msg += " — " + e.Hint
	}
	return msg + " (see docs/engine-role.md)"
}

// PrivilegedRole proves the connected role holds every access the
// requirement's tier needs against the target table. It can only be
// constructed by CheckPrivileges in this package. The owning role it
// carries is the catalog-resolved owner the copy-and-swap path will SET
// ROLE to for shadow objects. It records whether replication access was
// among the verified grants, so a later check that needs it can tell a
// proof that covers it from one that does not.
type PrivilegedRole struct {
	role            string
	owner           string
	tier            Tier
	logicalDecoding bool
}

// Role returns the connected role the checks ran as.
func (p PrivilegedRole) Role() string { return p.role }

// Owner returns the target table's owning role from the catalog.
func (p PrivilegedRole) Owner() string { return p.owner }

// Tier returns the tier the role was verified at.
func (p PrivilegedRole) Tier() Tier { return p.tier }

// LogicalDecoding reports whether the requirement the role was verified
// against included replication access.
func (p PrivilegedRole) LogicalDecoding() bool { return p.logicalDecoding }

// accessFacts is one catalog snapshot of every privilege fact the tier
// ladder consults, gathered in a single round trip so the checks cannot
// disagree about when they looked.
type accessFacts struct {
	role         string
	versionNum   int
	database     string
	schema       string
	table        string
	relkind      string
	owner        string
	canConnect   bool
	schemaUsage  bool
	schemaCreate bool
	ownerUsage   bool
	roleInherit  bool
	// ownerPrivileged reports an owner whose membership would hand the
	// engine role more than the owner's objects: see ownerPrivilegedSQL.
	ownerPrivileged bool
	// ownerSet reports that the role can already SET ROLE to the owner
	// (pg_has_role mode SET, PostgreSQL 16+; false below 16, where SET is
	// not a membership option). A refusal that re-grants the membership
	// keeps SET TRUE when it is, since a re-grant updates the options of
	// the membership it names and would otherwise clear it.
	ownerSet bool
}

// ownerPrivilegedSQL reports whether the role whose OID is $owner, or any
// role it is a member of, is a superuser, holds CREATEROLE, REPLICATION, or
// BYPASSRLS, is a predefined role other than pg_database_owner, or is
// rds_replication, which grants replication without the attribute. A
// membership in such an owner hands the engine role those powers, through
// SET ROLE where the membership allows it and through inheritance where a
// predefined role's privileges are concerned, so the refusal never advises
// it. pg_has_role in MEMBER mode follows every membership chain and counts
// the role itself.
const ownerPrivilegedSQL = `EXISTS (
		SELECT 1 FROM pg_catalog.pg_roles a
		WHERE (a.rolsuper OR a.rolcreaterole OR a.rolreplication OR a.rolbypassrls
		       OR (pg_catalog.starts_with(a.rolname::pg_catalog.text, 'pg_')
		           AND a.rolname OPERATOR(pg_catalog.<>) 'pg_database_owner')
		       OR a.rolname OPERATOR(pg_catalog.=) 'rds_replication')
		  AND pg_catalog.pg_has_role($owner, a.oid, 'MEMBER'))`

// ownerSetSQL reports whether the connected role can SET ROLE to the role
// whose OID is $owner. SET is a membership option only from PostgreSQL 16,
// and pg_has_role rejects the mode below it, so the probe answers false
// there without calling it.
const ownerSetSQL = `CASE WHEN pg_catalog.current_setting('server_version_num')::int OPERATOR(pg_catalog.>=) 160000
		     THEN pg_catalog.pg_has_role(current_user, $owner, 'SET') ELSE false END`

// CheckPrivileges verifies the connected role holds the access the
// requirement needs against schema.table (search_path resolution when
// schema is empty), per the tiered contract in docs/engine-role.md. A
// missing requirement is a *PrivilegeError naming the exact statement that
// would satisfy it; on success it returns the PrivilegedRole proof.
//
// It runs before CheckTable in the preflight order: a role that cannot see
// the target would otherwise report "table not found" and mask the real
// cause.
func CheckPrivileges(ctx context.Context, pool *pgxpool.Pool, schema, table string, req Requirement) (PrivilegedRole, error) {
	if req.Tier < TierConnect || req.Tier > TierCopyAndSwap {
		return PrivilegedRole{}, fmt.Errorf("requirement tier out of range: %d", int(req.Tier))
	}
	if req.LogicalDecoding && req.Tier != TierCopyAndSwap {
		return PrivilegedRole{}, fmt.Errorf("logical decoding is a copy-and-swap requirement; tier is %q", req.Tier)
	}

	facts, err := gatherAccessFacts(ctx, pool, schema, table)
	if err != nil {
		return PrivilegedRole{}, err
	}
	if err := checkTierLadder(ctx, pool, facts, req.Tier); err != nil {
		return PrivilegedRole{}, err
	}
	if req.LogicalDecoding {
		if err := checkReplicationAccess(ctx, pool, facts.role); err != nil {
			return PrivilegedRole{}, err
		}
	}
	return PrivilegedRole{role: facts.role, owner: facts.owner, tier: req.Tier, logicalDecoding: req.LogicalDecoding}, nil
}

// gatherAccessFacts resolves the target's schema and owner from the catalog
// and snapshots the tier 0–2 privilege facts in one query, so those checks
// cannot disagree with each other; the SET ROLE and replication probes are
// separate follow-up queries, each still fail-closed. A target that does not
// resolve is separated into its causes: schema missing, schema USAGE
// missing (which hides tables from search_path resolution), or the table
// genuinely absent. A target that resolves to something other than an
// ordinary or partitioned table is ErrNotTable — GRANT advice for a view
// would conflate the causes.
func gatherAccessFacts(ctx context.Context, pool *pgxpool.Pool, schema, table string) (accessFacts, error) {
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot answer a privilege probe true.
	q := `
		SELECT current_user::pg_catalog.text,
		       pg_catalog.current_setting('server_version_num')::int,
		       pg_catalog.current_database()::pg_catalog.text,
		       n.nspname::pg_catalog.text,
		       c.relkind::pg_catalog.text,
		       r.rolname::pg_catalog.text,
		       pg_catalog.has_database_privilege(current_user, pg_catalog.current_database(), 'CONNECT'),
		       pg_catalog.has_schema_privilege(current_user, n.nspname, 'USAGE'),
		       pg_catalog.has_schema_privilege(current_user, n.nspname, 'CREATE'),
		       pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE'),
		       (SELECT rolinherit FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) current_user),
		       ` + ownerPrivilegedFor("c.relowner") + `,
		       ` + ownerSetFor("c.relowner") + `
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) c.relowner
		WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(
			CASE WHEN $1 OPERATOR(pg_catalog.=) '' THEN pg_catalog.quote_ident($2)
			     ELSE pg_catalog.quote_ident($1) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident($2) END)`
	var f accessFacts
	err := pool.QueryRow(ctx, q, schema, table).Scan(
		&f.role, &f.versionNum, &f.database, &f.schema, &f.relkind, &f.owner,
		&f.canConnect, &f.schemaUsage, &f.schemaCreate, &f.ownerUsage, &f.roleInherit, &f.ownerPrivileged, &f.ownerSet)
	if errors.Is(err, pgx.ErrNoRows) {
		return accessFacts{}, unresolvedTargetCause(ctx, pool, schema, table)
	}
	// Resolving a qualified name checks schema USAGE and raises
	// insufficient_privilege when it is missing — the same unresolved
	// target as a NULL to_regclass, separated into its cause below.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateInsufficientPrivilege {
		return accessFacts{}, unresolvedTargetCause(ctx, pool, schema, table)
	}
	if err != nil {
		return accessFacts{}, fmt.Errorf("gather access facts for %s: %w", qualifiedName(schema, table), err)
	}
	// relkind 'r' is an ordinary table, 'p' a partitioned parent; anything
	// else (view, matview, foreign table, sequence) is refused as
	// ErrNotTable in the same snapshot, so a view owned by an unrelated
	// role is never refused with GRANT advice.
	if f.relkind != "r" && f.relkind != "p" {
		return accessFacts{}, fmt.Errorf("%w: %s has relkind %q", ErrNotTable, qualifiedName(schema, table), f.relkind)
	}
	f.table = table
	return f, nil
}

// unresolvedTargetCause separates why the target did not resolve. An
// unqualified name that fails is reported as not found: search_path
// resolution already skipped schemas the role lacks USAGE on, and there is
// no single schema to name in a refusal.
// rowQuerier is the single-row query capability shared by a pool and a
// transaction, so a check can run at plan time and again inside the
// transaction that acts on its proof.
type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func unresolvedTargetCause(ctx context.Context, db rowQuerier, schema, table string) error {
	if schema == "" {
		return fmt.Errorf("%w: %s", ErrTableNotFound, table)
	}
	// INV: CO-9 — pg_catalog-qualified like every privilege probe.
	const q = `
		SELECT current_user::pg_catalog.text,
		       pg_catalog.has_schema_privilege(current_user, n.nspname, 'USAGE')
		FROM pg_catalog.pg_namespace n
		WHERE n.nspname OPERATOR(pg_catalog.=) $1`
	var role string
	var usage bool
	err := db.QueryRow(ctx, q, schema).Scan(&role, &usage)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: schema %s does not exist", ErrTableNotFound, schema)
	}
	if err != nil {
		return fmt.Errorf("resolve schema %s: %w", schema, err)
	}
	if !usage {
		return schemaUsageRefusal(role, schema)
	}
	return fmt.Errorf("%w: %s", ErrTableNotFound, qualifiedName(schema, table))
}

// connectRefusal is the typed refusal for a role that cannot connect to
// the database, carrying the exact provisioning statement.
func connectRefusal(role, database string) *PrivilegeError {
	return &PrivilegeError{
		Tier:  TierConnect,
		Check: fmt.Sprintf("has_database_privilege(%s, %s, 'CONNECT')", role, database),
		Grant: fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s",
			pgx.Identifier{database}.Sanitize(), pgx.Identifier{role}.Sanitize()),
	}
}

// schemaUsageRefusal is the typed refusal for a role without USAGE on the
// schema, carrying the exact provisioning statement.
func schemaUsageRefusal(role, schema string) *PrivilegeError {
	return &PrivilegeError{
		Tier:  TierConnect,
		Check: fmt.Sprintf("has_schema_privilege(%s, %s, 'USAGE')", role, schema),
		Grant: fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
			pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()),
	}
}

// checkTierLadder walks the contract's tiers bottom-up to the requirement
// and returns the first missing access as a typed refusal. Bottom-up order
// makes the refusal actionable: the operator fixes the foundational grant
// before discovering the next one.
//
// INV: ST-6 — every role prerequisite the plan's tier needs is verified
// against the live catalog before the first write; a missing access is a
// typed refusal naming the exact provisioning statement, never a
// mid-change server error.
func checkTierLadder(ctx context.Context, pool *pgxpool.Pool, f accessFacts, tier Tier) error {
	if !f.canConnect {
		return connectRefusal(f.role, f.database)
	}
	if !f.schemaUsage {
		return schemaUsageRefusal(f.role, f.schema)
	}
	if tier < TierAlterInPlace {
		return nil
	}
	if !f.ownerUsage {
		return ownerMembershipRefusal(f, "table", tier)
	}
	if tier < TierIndexBuild {
		return nil
	}
	// The owner is the grantee: index builds and shadow objects live in
	// the schema, and granting CREATE to the owning role covers every
	// member engine role at once (the engine inherits it through the
	// Tier 1 membership).
	if !f.schemaCreate {
		return &PrivilegeError{
			Tier:  TierIndexBuild,
			Check: fmt.Sprintf("has_schema_privilege(%s, %s, 'CREATE')", f.role, f.schema),
			Grant: fmt.Sprintf("GRANT CREATE ON SCHEMA %s TO %s",
				pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{f.owner}.Sanitize()),
			Hint: "the grant targets the owning role, which the engine inherits through its Tier 1 membership",
		}
	}
	if tier < TierCopyAndSwap {
		return nil
	}
	return checkSetRoleAccess(ctx, pool, f, transferTableRemedy)
}

// ownerPrivilegedFor is ownerPrivilegedSQL for the owner OID that column
// holds.
func ownerPrivilegedFor(column string) string {
	return strings.ReplaceAll(ownerPrivilegedSQL, "$owner", column)
}

// ownerSetFor is ownerSetSQL for the owner OID that column holds.
func ownerSetFor(column string) string {
	return strings.ReplaceAll(ownerSetSQL, "$owner", column)
}

// ownerMembershipRefusal is the Tier-1 refusal, with the remediation that
// actually flips the failed check. On 14–15 inheritance is the grantee's
// rolinherit attribute, which GRANT cannot change — a NOINHERIT role must
// be altered first, then (a re-run refuses again if needed) granted
// membership. On 16+ inheritance is a membership option, and GRANT ...
// WITH INHERIT TRUE both creates a missing membership and updates an
// existing non-inheriting one; it grants SET only when tier, the rung the
// run needs, is copy-and-swap, the one rung that runs SET ROLE, or when the
// role can already SET ROLE to the owner, since the re-grant would
// otherwise clear that option on the membership it updates. object
// names what the role would alter, a table or a type, for the ownership
// transfer the hint offers instead.
//
// An owner that is a superuser or holds administrative power is never
// offered as a membership, on any version: the grant would hand the engine
// role that power. The refusal names the transfer alone.
func ownerMembershipRefusal(f accessFacts, object string, tier Tier) *PrivilegeError {
	const pg16 = 160000
	transferSQL := fmt.Sprintf("ALTER %s %s.%s OWNER TO <role>",
		strings.ToUpper(object), pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{f.table}.Sanitize())
	check := fmt.Sprintf("pg_has_role(%s, %s, 'USAGE')", f.role, f.owner)
	if f.ownerPrivileged {
		return &PrivilegeError{
			Tier:  TierAlterInPlace,
			Check: check,
			Grant: transferSQL,
			Hint: fmt.Sprintf("the %s's owner %s is a superuser or holds administrative power, and membership in it would hand the engine role that power; transfer the %s to an ordinary role, then grant the engine role membership in that role",
				object, f.owner, object),
		}
	}
	// A membership grant hands the role every privilege the owner holds on
	// any version, so the transfer is offered wherever the grant is.
	transfer := fmt.Sprintf("the grant hands the engine role every privilege %s holds; to give it only this %s, transfer the %s instead: %s",
		f.owner, object, object, transferSQL)
	if f.versionNum >= pg16 {
		set := "FALSE"
		if tier >= TierCopyAndSwap || f.ownerSet {
			set = "TRUE"
		}
		return &PrivilegeError{
			Tier:  TierAlterInPlace,
			Check: check,
			Grant: fmt.Sprintf("GRANT %s TO %s WITH INHERIT TRUE, SET %s",
				pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize(), set),
			Hint: transfer,
		}
	}
	if !f.roleInherit {
		return &PrivilegeError{
			Tier:  TierAlterInPlace,
			Check: fmt.Sprintf("pg_roles.rolinherit for %s", f.role),
			Grant: fmt.Sprintf("ALTER ROLE %s INHERIT", pgx.Identifier{f.role}.Sanitize()),
		}
	}
	return &PrivilegeError{
		Tier:  TierAlterInPlace,
		Check: check,
		Grant: fmt.Sprintf("GRANT %s TO %s",
			pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()),
		Hint: transfer,
	}
}

// checkSetRoleAccess verifies the owning-role membership is usable with
// SET ROLE, so objects created under it are born with the correct owner.
// PostgreSQL 16 made SET a distinct membership option (pg_has_role mode
// 'SET'); on 14–15 SET ROLE consults plain membership, which every caller
// has already proved before reaching this rung — the ownership ladder
// through an inheriting USAGE chain (a membership chain), the create path
// through MEMBER directly — so there is nothing further to check below 16.
//
// A missing SET option on an owner that holds administrative power is never
// remedied with the grant: SET ROLE is how the engine role would reach a
// superuser's or a CREATEROLE role's power, which inheritance does not pass
// on. remedy names what to do instead, for the caller's kind of owner.
func checkSetRoleAccess(ctx context.Context, pool *pgxpool.Pool, f accessFacts, remedy privilegedOwnerRemedy) error {
	const pg16 = 160000
	if f.versionNum < pg16 {
		return nil
	}
	// INV: CO-9 — pg_catalog-qualified like every privilege probe.
	q := `
		SELECT pg_catalog.pg_has_role(current_user, r.oid, 'SET'),
		       ` + ownerPrivilegedFor("r.oid") + `
		FROM pg_catalog.pg_roles r
		WHERE r.rolname OPERATOR(pg_catalog.=) $1`
	var canSet, privileged bool
	if err := pool.QueryRow(ctx, q, f.owner).Scan(&canSet, &privileged); err != nil {
		return fmt.Errorf("check SET ROLE access to %s: %w", f.owner, err)
	}
	if !canSet && privileged {
		return remedy(f)
	}
	if !canSet {
		return &PrivilegeError{
			Tier:  TierCopyAndSwap,
			Check: fmt.Sprintf("pg_has_role(%s, %s, 'SET')", f.role, f.owner),
			Grant: fmt.Sprintf("GRANT %s TO %s WITH SET TRUE",
				pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()),
		}
	}
	return nil
}

// privilegedOwnerRemedy builds the refusal for a missing SET option on an
// owner that holds administrative power, naming what to do instead of the
// grant.
type privilegedOwnerRemedy func(accessFacts) *PrivilegeError

// transferTableRemedy is the copy-and-swap rung's remedy: the table moves to
// an ordinary role, which the engine role is then made a member of.
func transferTableRemedy(f accessFacts) *PrivilegeError {
	return &PrivilegeError{
		Tier:  TierCopyAndSwap,
		Check: fmt.Sprintf("pg_has_role(%s, %s, 'SET')", f.role, f.owner),
		Grant: fmt.Sprintf("ALTER TABLE %s.%s OWNER TO <role>",
			pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{f.table}.Sanitize()),
		Hint: fmt.Sprintf("the table's owner %s is a superuser or holds administrative power, and SET ROLE to it would hand the engine role that power; transfer the table to an ordinary role, then grant the engine role membership in that role WITH INHERIT TRUE, SET TRUE",
			f.owner),
	}
}

// checkReplicationAccess verifies the role can open a logical replication
// connection. Where the rds_replication role exists (Aurora/RDS) membership
// in it is the mechanism; elsewhere the REPLICATION role attribute is. The
// role attribute also satisfies the check on a cluster that happens to have
// an rds_replication role: either grant proves the same capability.
func checkReplicationAccess(ctx context.Context, pool *pgxpool.Pool, role string) error {
	// INV: CO-9 — pg_catalog-qualified like every privilege probe.
	const q = `
		SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) 'rds_replication'),
		       COALESCE((SELECT pg_catalog.pg_has_role(current_user, r.oid, 'MEMBER')
		                   FROM pg_catalog.pg_roles r WHERE r.rolname OPERATOR(pg_catalog.=) 'rds_replication'), false),
		       (SELECT rolreplication FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) current_user)`
	var rdsRoleExists, rdsMember, replicationAttr bool
	if err := pool.QueryRow(ctx, q).Scan(&rdsRoleExists, &rdsMember, &replicationAttr); err != nil {
		return fmt.Errorf("check replication access for %s: %w", role, err)
	}
	if replicationAttr {
		return nil
	}
	if rdsRoleExists {
		if rdsMember {
			return nil
		}
		return &PrivilegeError{
			Tier:  TierCopyAndSwap,
			Check: fmt.Sprintf("pg_has_role(%s, rds_replication, 'MEMBER')", role),
			Grant: fmt.Sprintf("GRANT rds_replication TO %s", pgx.Identifier{role}.Sanitize()),
		}
	}
	return &PrivilegeError{
		Tier:  TierCopyAndSwap,
		Check: fmt.Sprintf("pg_roles.rolreplication for %s", role),
		Grant: fmt.Sprintf("ALTER ROLE %s WITH REPLICATION", pgx.Identifier{role}.Sanitize()),
	}
}
