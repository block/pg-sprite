package preflight_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// connectAs returns a pool connected to the same server and database as
// serverURL but authenticated as the given role.
func connectAs(t *testing.T, serverURL, role, password string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(serverURL)
	require.NoError(t, err, "parse server URL")
	u.User = url.UserPassword(role, password)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err, "connect as %s", role)
	t.Cleanup(pool.Close)
	return pool
}

// privilegeFixture is the ladder scenario from docs/engine-role.md: a
// schema owned by the superuser, a table owned by an application role, and
// an engine role that starts with no access beyond LOGIN.
type privilegeFixture struct {
	admin  *pgxpool.Pool // superuser, provisions grants
	engine *pgxpool.Pool // the role under test
	schema string
	owner  string
	role   string // the engine role's name
}

func newPrivilegeFixture(t *testing.T) privilegeFixture {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	owner := testutil.NewRole(t, admin, "NOLOGIN")
	const password = "engine-test-password"
	role := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	schema := testutil.NewSchema(t, admin)
	_, err = admin.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.target (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	_, err = admin.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.target OWNER TO %s",
		schema, pgx.Identifier{owner}.Sanitize()))
	require.NoError(t, err)

	engine := connectAs(t, serverURL, role, password)
	return privilegeFixture{admin: admin, engine: engine, schema: schema, owner: owner, role: role}
}

func (f privilegeFixture) grant(t *testing.T, stmt string) {
	t.Helper()
	_, err := f.admin.Exec(t.Context(), stmt)
	require.NoError(t, err, "provision: %s", stmt)
}

func serverVersionNum(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var v string
	require.NoError(t, pool.QueryRow(t.Context(), "SHOW server_version_num").Scan(&v))
	n, err := strconv.Atoi(v)
	require.NoError(t, err)
	return n
}

// The ladder from docs/engine-role.md, walked bottom-up: each missing
// access is a typed refusal naming the exact provisioning statement, and
// applying exactly that statement unlocks the next rung.
func TestCheckPrivilegesWalksTheTierLadder(t *testing.T) {
	f := newPrivilegeFixture(t)
	ctx := t.Context()

	// Tier 0: no USAGE on the schema yet.
	_, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierConnect})
	var privErr *preflight.PrivilegeError
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierConnect, privErr.Tier)
	assert.Equal(t, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
		pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{f.role}.Sanitize()), privErr.Grant)

	// Applying exactly the refusal's grant satisfies Tier 0.
	f.grant(t, privErr.Grant)
	proof, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierConnect})
	require.NoError(t, err)
	assert.Equal(t, f.role, proof.Role())
	assert.Equal(t, f.owner, proof.Owner())
	assert.Equal(t, preflight.TierConnect, proof.Tier())

	// Tier 1: no owning-role membership yet. On 16+ the grant carries the
	// INHERIT option explicitly, so it also repairs a non-inheriting
	// membership; below 16 inheritance is the grantee's role attribute.
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierAlterInPlace})
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierAlterInPlace, privErr.Tier)
	tier1Grant := fmt.Sprintf("GRANT %s TO %s",
		pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize())
	if serverVersionNum(t, f.admin) >= 160000 {
		tier1Grant += " WITH INHERIT TRUE"
	}
	assert.Equal(t, tier1Grant, privErr.Grant)

	f.grant(t, privErr.Grant)
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierAlterInPlace})
	require.NoError(t, err)

	// Tier 2: the owning role lacks CREATE on the superuser-owned schema.
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierIndexBuild})
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierIndexBuild, privErr.Tier)
	assert.Equal(t, fmt.Sprintf("GRANT CREATE ON SCHEMA %s TO %s",
		pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{f.owner}.Sanitize()), privErr.Grant)

	// The grant goes to the owner; the engine inherits it via membership.
	f.grant(t, privErr.Grant)
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierIndexBuild})
	require.NoError(t, err)

	// Tier 3: a default membership grant is SET-capable on every
	// supported major, so copy-and-swap admits without further grants.
	proof, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	assert.Equal(t, preflight.TierCopyAndSwap, proof.Tier())
}

// A NOINHERIT engine role's Tier-1 refusal must name a remediation that
// actually flips the failed check: re-running a plain GRANT changes
// nothing when the membership already exists but does not inherit. Below
// 16 the fix is the rolinherit role attribute; on 16+ it is the
// membership's INHERIT option.
func TestCheckPrivilegesRemediatesNonInheritingMembership(t *testing.T) {
	f := newPrivilegeFixture(t)
	ctx := t.Context()
	f.grant(t, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
		f.schema, pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf("ALTER ROLE %s NOINHERIT", pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf("GRANT %s TO %s",
		pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()))

	// The membership exists but confers nothing without inheritance.
	_, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "target",
		preflight.Requirement{Tier: preflight.TierAlterInPlace})
	var privErr *preflight.PrivilegeError
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierAlterInPlace, privErr.Tier)
	if serverVersionNum(t, f.admin) >= 160000 {
		assert.Equal(t, fmt.Sprintf("GRANT %s TO %s WITH INHERIT TRUE",
			pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()), privErr.Grant)
	} else {
		assert.Equal(t, fmt.Sprintf("ALTER ROLE %s INHERIT",
			pgx.Identifier{f.role}.Sanitize()), privErr.Grant)
	}

	// Applying exactly the refusal's remediation flips the check.
	f.grant(t, privErr.Grant)
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target",
		preflight.Requirement{Tier: preflight.TierAlterInPlace})
	require.NoError(t, err)
}

// A view owned by a role the engine lacks membership in is ErrNotTable,
// never a privilege refusal: GRANT advice for a non-table would send the
// operator through a useless provisioning loop before the real answer.
func TestCheckPrivilegesRefusesViewAsNotTable(t *testing.T) {
	f := newPrivilegeFixture(t)
	ctx := t.Context()
	f.grant(t, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
		f.schema, pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf("CREATE VIEW %s.v AS SELECT 1 AS one", f.schema))
	f.grant(t, fmt.Sprintf("ALTER VIEW %s.v OWNER TO %s",
		f.schema, pgx.Identifier{f.owner}.Sanitize()))

	_, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "v",
		preflight.Requirement{Tier: preflight.TierAlterInPlace})
	assert.ErrorIs(t, err, preflight.ErrNotTable)
	var privErr *preflight.PrivilegeError
	assert.False(t, errors.As(err, &privErr), "a non-table must not surface as a privilege refusal")
}

// Unqualified targets resolve through search_path: migrate passes them
// straight through, so the facts query's empty-schema branch and the
// unqualified not-found return are production-reachable and pinned here.
func TestCheckPrivilegesResolvesUnqualifiedNames(t *testing.T) {
	f := newPrivilegeFixture(t)
	ctx := t.Context()
	table := testutil.NewPublicTable(t, f.admin, "(id int PRIMARY KEY)")
	f.grant(t, fmt.Sprintf("ALTER TABLE public.%s OWNER TO %s",
		table, pgx.Identifier{f.owner}.Sanitize()))

	// public carries USAGE for every role, so the unqualified name
	// resolves; the engine still lacks owning-role membership.
	_, err := preflight.CheckPrivileges(ctx, f.engine, "", table,
		preflight.Requirement{Tier: preflight.TierAlterInPlace})
	var privErr *preflight.PrivilegeError
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierAlterInPlace, privErr.Tier)

	f.grant(t, privErr.Grant)
	proof, err := preflight.CheckPrivileges(ctx, f.engine, "", table,
		preflight.Requirement{Tier: preflight.TierAlterInPlace})
	require.NoError(t, err)
	assert.Equal(t, f.owner, proof.Owner())

	// A genuinely absent unqualified name is not-found: search_path
	// resolution already skipped what the role cannot see, so there is
	// no single schema for a refusal to name.
	_, err = preflight.CheckPrivileges(ctx, f.engine, "", "no_such_table_anywhere",
		preflight.Requirement{Tier: preflight.TierConnect})
	assert.ErrorIs(t, err, preflight.ErrTableNotFound)
}

// PostgreSQL 16 made SET a distinct membership option: a WITH SET FALSE
// membership still passes plain ownership checks but cannot SET ROLE, so
// copy-and-swap must refuse it with the SET TRUE re-grant.
func TestCheckPrivilegesRefusesSetFalseMembership(t *testing.T) {
	f := newPrivilegeFixture(t)
	if v := serverVersionNum(t, f.admin); v < 160000 {
		t.Skipf("membership SET option needs PostgreSQL 16+, server is %d", v)
	}
	ctx := t.Context()
	f.grant(t, fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA %s TO %s",
		f.schema, pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf("GRANT %s TO %s WITH SET FALSE",
		pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()))

	// In-place ALTER is satisfied: ownership checks pass through
	// inheritance regardless of the SET option.
	_, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierAlterInPlace})
	require.NoError(t, err)

	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	var privErr *preflight.PrivilegeError
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierCopyAndSwap, privErr.Tier)
	assert.Equal(t, fmt.Sprintf("GRANT %s TO %s WITH SET TRUE",
		pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()), privErr.Grant)

	f.grant(t, privErr.Grant)
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
}

// Replication access has two mechanisms: rds_replication membership where
// that role exists (Aurora/RDS), the REPLICATION role attribute otherwise.
// The refusal names the mechanism the cluster actually uses.
func TestCheckPrivilegesReplicationAccess(t *testing.T) {
	if os.Getenv("PG_DSN") != "" {
		t.Skip("creates the cluster-level rds_replication role; container-only")
	}
	f := newPrivilegeFixture(t)
	ctx := t.Context()
	f.grant(t, fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA %s TO %s",
		f.schema, pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf("GRANT %s TO %s",
		pgx.Identifier{f.owner}.Sanitize(), pgx.Identifier{f.role}.Sanitize()))
	cdc := preflight.Requirement{Tier: preflight.TierCopyAndSwap, LogicalDecoding: true}

	// Self-managed flavor: no rds_replication role exists, so the
	// REPLICATION attribute is the mechanism.
	_, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", cdc)
	var privErr *preflight.PrivilegeError
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, fmt.Sprintf("ALTER ROLE %s WITH REPLICATION",
		pgx.Identifier{f.role}.Sanitize()), privErr.Grant)

	// Aurora/RDS flavor: with an rds_replication role present, membership
	// is the mechanism the refusal names.
	_, err = f.admin.Exec(ctx, "CREATE ROLE rds_replication NOLOGIN")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.admin.Exec(context.WithoutCancel(t.Context()), "DROP ROLE IF EXISTS rds_replication")
		if err != nil {
			t.Logf("drop rds_replication: %v", err)
		}
	})
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", cdc)
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, fmt.Sprintf("GRANT rds_replication TO %s",
		pgx.Identifier{f.role}.Sanitize()), privErr.Grant)

	f.grant(t, privErr.Grant)
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", cdc)
	require.NoError(t, err)

	// The REPLICATION attribute satisfies the check even where the
	// rds_replication role exists: either grant proves the capability.
	f.grant(t, fmt.Sprintf("REVOKE rds_replication FROM %s", pgx.Identifier{f.role}.Sanitize()))
	f.grant(t, fmt.Sprintf("ALTER ROLE %s WITH REPLICATION", pgx.Identifier{f.role}.Sanitize()))
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", cdc)
	require.NoError(t, err)
}

// A role that cannot see the target must learn the real cause: schema
// USAGE missing is a privilege refusal, not "table not found" — and a
// genuinely absent table stays a not-found even for a privileged role.
func TestCheckPrivilegesSeparatesUnresolvedCauses(t *testing.T) {
	f := newPrivilegeFixture(t)
	ctx := t.Context()

	_, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "absent", preflight.Requirement{Tier: preflight.TierConnect})
	var privErr *preflight.PrivilegeError
	if assert.NotErrorIs(t, err, preflight.ErrTableNotFound, "missing USAGE must not masquerade as not-found") {
		require.ErrorAs(t, err, &privErr)
		assert.Equal(t, preflight.TierConnect, privErr.Tier)
	}

	f.grant(t, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
		f.schema, pgx.Identifier{f.role}.Sanitize()))
	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "absent", preflight.Requirement{Tier: preflight.TierConnect})
	assert.ErrorIs(t, err, preflight.ErrTableNotFound)

	_, err = preflight.CheckPrivileges(ctx, f.engine, "no_such_schema", "absent", preflight.Requirement{Tier: preflight.TierConnect})
	assert.ErrorIs(t, err, preflight.ErrTableNotFound)
}

func TestCheckPrivilegesValidatesRequirement(t *testing.T) {
	f := newPrivilegeFixture(t)
	ctx := t.Context()

	_, err := preflight.CheckPrivileges(ctx, f.engine, f.schema, "target", preflight.Requirement{Tier: preflight.Tier(99)})
	require.Error(t, err)
	assert.NotErrorIs(t, err, preflight.ErrTableNotFound)

	_, err = preflight.CheckPrivileges(ctx, f.engine, f.schema, "target",
		preflight.Requirement{Tier: preflight.TierIndexBuild, LogicalDecoding: true})
	require.Error(t, err, "logical decoding below copy-and-swap is a caller bug")
}
