package preflight

import (
	"context"
	"errors"
	"fmt"

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
	// the database and USAGE on the target schema.
	TierConnect Tier = iota
	// TierAlterInPlace covers owner-gated in-place ALTER TABLE (the
	// instant and fast native paths): inheritable membership in the
	// owning role.
	TierAlterInPlace
	// TierIndexBuild covers CREATE INDEX [CONCURRENTLY]: CREATE on the
	// target schema on top of owning-role membership.
	TierIndexBuild
	// TierCopyAndSwap covers shadow-object creation: membership usable
	// with SET ROLE, so shadow objects are born with the correct owner.
	TierCopyAndSwap
)

// String names the tier's capability for refusal messages.
func (t Tier) String() string {
	switch t {
	case TierConnect:
		return "connect and resolve the target"
	case TierAlterInPlace:
		return "in-place ALTER TABLE"
	case TierIndexBuild:
		return "index builds"
	case TierCopyAndSwap:
		return "copy-and-swap"
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
	// Check is the catalog predicate that returned false.
	Check string
	// Grant is the exact statement that would satisfy the check.
	Grant string
}

// Error implements the error interface.
func (e *PrivilegeError) Error() string {
	return fmt.Sprintf("engine role lacks access for %s: %s is false; provision with: %s (see docs/engine-role.md)",
		e.Tier, e.Check, e.Grant)
}

// PrivilegedRole proves the connected role holds every access the
// requirement's tier needs against the target table. It can only be
// constructed by CheckPrivileges in this package. The owning role it
// carries is the catalog-resolved owner the copy-and-swap path will SET
// ROLE to for shadow objects.
type PrivilegedRole struct {
	role  string
	owner string
	tier  Tier
}

// Role returns the connected role the checks ran as.
func (p PrivilegedRole) Role() string { return p.role }

// Owner returns the target table's owning role from the catalog.
func (p PrivilegedRole) Owner() string { return p.owner }

// Tier returns the tier the role was verified at.
func (p PrivilegedRole) Tier() Tier { return p.tier }

// accessFacts is one catalog snapshot of every privilege fact the tier
// ladder consults, gathered in a single round trip so the checks cannot
// disagree about when they looked.
type accessFacts struct {
	role         string
	versionNum   int
	database     string
	schema       string
	owner        string
	canConnect   bool
	schemaUsage  bool
	schemaCreate bool
	ownerUsage   bool
	ownerMember  bool
}

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
	return PrivilegedRole{role: facts.role, owner: facts.owner, tier: req.Tier}, nil
}

// gatherAccessFacts resolves the target's schema and owner from the catalog
// and snapshots every privilege fact in one query. A target that does not
// resolve is separated into its causes: schema missing, schema USAGE
// missing (which hides tables from search_path resolution), or the table
// genuinely absent.
func gatherAccessFacts(ctx context.Context, pool *pgxpool.Pool, schema, table string) (accessFacts, error) {
	const q = `
		SELECT current_user::text,
		       current_setting('server_version_num')::int,
		       current_database()::text,
		       n.nspname::text,
		       r.rolname::text,
		       has_database_privilege(current_user, current_database(), 'CONNECT'),
		       has_schema_privilege(current_user, n.nspname, 'USAGE'),
		       has_schema_privilege(current_user, n.nspname, 'CREATE'),
		       pg_has_role(current_user, c.relowner, 'USAGE'),
		       pg_has_role(current_user, c.relowner, 'MEMBER')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_roles r ON r.oid = c.relowner
		WHERE c.oid = to_regclass(
			CASE WHEN $1 = '' THEN quote_ident($2)
			     ELSE quote_ident($1) || '.' || quote_ident($2) END)`
	var f accessFacts
	err := pool.QueryRow(ctx, q, schema, table).Scan(
		&f.role, &f.versionNum, &f.database, &f.schema, &f.owner,
		&f.canConnect, &f.schemaUsage, &f.schemaCreate, &f.ownerUsage, &f.ownerMember)
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
	return f, nil
}

// unresolvedTargetCause separates why the target did not resolve. An
// unqualified name that fails is reported as not found: search_path
// resolution already skipped schemas the role lacks USAGE on, and there is
// no single schema to name in a refusal.
func unresolvedTargetCause(ctx context.Context, pool *pgxpool.Pool, schema, table string) error {
	if schema == "" {
		return fmt.Errorf("%w: %s", ErrTableNotFound, table)
	}
	const q = `
		SELECT current_user::text,
		       has_schema_privilege(current_user, n.nspname, 'USAGE')
		FROM pg_namespace n
		WHERE n.nspname = $1`
	var role string
	var usage bool
	err := pool.QueryRow(ctx, q, schema).Scan(&role, &usage)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: schema %s does not exist", ErrTableNotFound, schema)
	}
	if err != nil {
		return fmt.Errorf("resolve schema %s: %w", schema, err)
	}
	if !usage {
		return &PrivilegeError{
			Tier:  TierConnect,
			Check: fmt.Sprintf("has_schema_privilege(%s, %s, 'USAGE')", role, schema),
			Grant: fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
				pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()),
		}
	}
	return fmt.Errorf("%w: %s", ErrTableNotFound, qualifiedName(schema, table))
}

// checkTierLadder walks the contract's tiers bottom-up to the requirement
// and returns the first missing access as a typed refusal. Bottom-up order
// makes the refusal actionable: the operator fixes the foundational grant
// before discovering the next one.
func checkTierLadder(ctx context.Context, pool *pgxpool.Pool, f accessFacts, tier Tier) error {
	if !f.canConnect {
		return &PrivilegeError{
			Tier:  TierConnect,
			Check: fmt.Sprintf("has_database_privilege(%s, %s, 'CONNECT')", f.role, f.database),
			Grant: fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s",
				pgx.Identifier{f.database}.Sanitize(), pgx.Identifier{f.role}.Sanitize()),
		}
	}
	if !f.schemaUsage {
		return &PrivilegeError{
			Tier:  TierConnect,
			Check: fmt.Sprintf("has_schema_privilege(%s, %s, 'USAGE')", f.role, f.schema),
			Grant: fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
				pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{f.role}.Sanitize()),
		}
	}
	if tier < TierAlterInPlace {
		return nil
	}
	if !f.ownerUsage {
		return &PrivilegeError{
			Tier:  TierAlterInPlace,
			Check: fmt.Sprintf("pg_has_role(%s, %s, 'USAGE')", f.role, f.owner),
			Grant: fmt.Sprintf("GRANT %s TO %s",
				pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()),
		}
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
		}
	}
	if tier < TierCopyAndSwap {
		return nil
	}
	return checkSetRoleAccess(ctx, pool, f)
}

// checkSetRoleAccess verifies the owning-role membership is usable with
// SET ROLE, so shadow objects are born with the correct owner. PostgreSQL
// 16 made SET a distinct membership option (pg_has_role mode 'SET'); on
// 14–15 plain membership is what SET ROLE consults.
func checkSetRoleAccess(ctx context.Context, pool *pgxpool.Pool, f accessFacts) error {
	const pg16 = 160000
	if f.versionNum < pg16 {
		if !f.ownerMember {
			return &PrivilegeError{
				Tier:  TierCopyAndSwap,
				Check: fmt.Sprintf("pg_has_role(%s, %s, 'MEMBER')", f.role, f.owner),
				Grant: fmt.Sprintf("GRANT %s TO %s",
					pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()),
			}
		}
		return nil
	}
	const q = `SELECT pg_has_role(current_user, r.oid, 'SET') FROM pg_roles r WHERE r.rolname = $1`
	var canSet bool
	if err := pool.QueryRow(ctx, q, f.owner).Scan(&canSet); err != nil {
		return fmt.Errorf("check SET ROLE access to %s: %w", f.owner, err)
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

// checkReplicationAccess verifies the role can open a logical replication
// connection. Where the rds_replication role exists (Aurora/RDS) membership
// in it is the mechanism; elsewhere the REPLICATION role attribute is. The
// role attribute also satisfies the check on a cluster that happens to have
// an rds_replication role: either grant proves the same capability.
func checkReplicationAccess(ctx context.Context, pool *pgxpool.Pool, role string) error {
	const q = `
		SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rds_replication'),
		       COALESCE((SELECT pg_has_role(current_user, r.oid, 'MEMBER')
		                   FROM pg_roles r WHERE r.rolname = 'rds_replication'), false),
		       (SELECT rolreplication FROM pg_roles WHERE rolname = current_user)`
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
