package schemachange_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// INV: CO-9. The shadow builder reads the table's ST-5 facts (comment, ACLs,
// policies) on a transaction path that puts pg_catalog implicitly first, which
// protects every name the catalog spells exactly. It does not protect a call
// that PostgreSQL resolves by argument types: an impostor function or
// operator in the schema whose signature fits the call better than the
// catalog's wins, because overload resolution runs before search_path order.
// Each subtest plants one such impostor in the target schema, first proves it
// answers on the build path, and then requires BuildShadow to record the
// source's real facts anyway.
func TestBuildShadowFidelityReadsIgnoreImpostorCatalogNames(t *testing.T) {
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	setup, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(setup.Close)
	// Roles before the schemas, so each schema (and the grants and policies in
	// it) is dropped before the roles are.
	reader := testutil.NewRole(t, setup, "NOLOGIN")
	other := testutil.NewRole(t, setup, "NOLOGIN")
	// Each subtest gets a pool of its own: a pooled connection keeps prepared
	// statements, and one prepared before another subtest's impostors existed
	// would answer from the catalog regardless of the code under test.
	newFixture := func(t *testing.T) shadowFixture {
		t.Helper()
		pool, err := dbconn.NewPool(t.Context(), cfg)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		return shadowFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	}

	// obj_description(oid, name) is the catalog's comment reader; an impostor
	// taking (oid, text) fits the string literal 'pg_class' better, so the
	// unqualified call would record the impostor's text as the table comment
	// and the shadow would carry it.
	t.Run("table comment", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %s.accounts (
				id bigint PRIMARY KEY,
				balance numeric
			)`)
		f.exec(t, `COMMENT ON TABLE %s.accounts IS 'ledger of customer balances'`)
		f.exec(t, `
			CREATE FUNCTION %s.obj_description(oid, text) RETURNS text
			LANGUAGE sql AS $$ SELECT 'impostor comment'::text $$`)
		requireImpostorAnswersOnBuildPath(t, f,
			`SELECT obj_description('%s.accounts'::regclass::oid, 'pg_class')`, "impostor comment")

		built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ADD COLUMN note text`)
		require.NoError(t, err)

		assert.Equal(t, "ledger of customer balances", built.Fidelity().Comment)
		assert.Equal(t, "ledger of customer balances", f.comment(t, built.ShadowTable()))
	})

	// The builder renders COMMENT ON through format(template, VARIADIC text[]);
	// an impostor format(text, text[]) fits the call exactly, so the statement
	// the shadow runs would be the impostor's, not the server's quoting.
	t.Run("comment rendering", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %s.accounts (
				id bigint PRIMARY KEY,
				balance numeric
			)`)
		f.exec(t, `COMMENT ON TABLE %s.accounts IS 'ledger of customer balances'`)
		f.exec(t, `
			CREATE FUNCTION %s.format(text, text[]) RETURNS text
			LANGUAGE sql AS $$ SELECT 'COMMENT ON TABLE ' || $2[1] || ' IS ''impostor render''' $$`)
		requireImpostorAnswersOnBuildPath(t, f,
			`SELECT format('COMMENT ON TABLE %I IS %L', VARIADIC ARRAY['t', 'c']::text[])`,
			"COMMENT ON TABLE t IS 'impostor render'")

		built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ADD COLUMN note text`)
		require.NoError(t, err)

		assert.Equal(t, "ledger of customer balances", f.comment(t, built.ShadowTable()))
	})

	// A grantee OID is compared with the integer literal 0 to spot PUBLIC.
	// The catalog has only oid = oid, reached through an implicit cast of the
	// literal; an impostor oid = integer matches the call exactly and would
	// report every grantee, a real role included, as PUBLIC, so the shadow
	// would be granted to everyone.
	t.Run("table and column grants", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %s.accounts (
				id bigint PRIMARY KEY,
				secret text
			)`)
		f.exec(t, `GRANT SELECT ON %s.accounts TO `+pgx.Identifier{reader}.Sanitize())
		f.exec(t, `GRANT UPDATE (secret) ON %s.accounts TO `+pgx.Identifier{reader}.Sanitize())
		f.exec(t, `
			CREATE FUNCTION %s.always_true(oid, integer) RETURNS boolean
			LANGUAGE sql AS $$ SELECT true $$`)
		f.exec(t, `CREATE OPERATOR %s.= (LEFTARG = oid, RIGHTARG = integer, FUNCTION = %s.always_true)`)
		requireImpostorAnswersOnBuildPath(t, f, `SELECT (1::oid = 0)::text`, "true")

		built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ADD COLUMN note text`)
		require.NoError(t, err)

		for _, g := range built.Fidelity().Grants {
			assert.False(t, g.Public, "no table grant of the source is to PUBLIC: %+v", g)
		}
		assert.Contains(t, built.Fidelity().Grants, schemachange.Grant{Privilege: "SELECT", Grantee: reader})
		assert.Equal(t, []schemachange.ColumnGrant{
			{Column: "secret", Grant: schemachange.Grant{Privilege: "UPDATE", Grantee: reader}},
		}, built.Fidelity().ColumnGrants)
		sourceTable, sourceColumn := f.aclByOID(t, "accounts")
		shadowTable, shadowColumn := f.aclByOID(t, built.ShadowTable())
		assert.Equal(t, sourceTable, shadowTable, "the shadow's table ACL is the source's")
		assert.Equal(t, sourceColumn, shadowColumn, "the shadow's column ACL is the source's")
	})

	// A table with no stored ACL holds its owner's default privileges, which
	// the builder reads from acldefault('r', owner). The kind code is passed
	// as an untyped parameter, so an impostor acldefault(text, oid) is
	// preferred over the catalog's ("char", oid); an impostor that reports
	// only SELECT to PUBLIC would be recorded as the source's whole ACL.
	t.Run("owner default privileges", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %s.accounts (
				id bigint PRIMARY KEY,
				balance numeric
			)`)
		f.exec(t, `
			CREATE FUNCTION %s.acldefault(text, oid) RETURNS aclitem[]
			LANGUAGE sql AS $$ SELECT ARRAY[makeaclitem(0, $2, 'SELECT', false)] $$`)
		requireImpostorAnswersOnBuildPath(t, f,
			`SELECT (SELECT count(*) FROM aclexplode(acldefault('r', 10::oid)))::text`, "1")

		built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ADD COLUMN note text`)
		require.NoError(t, err)

		var owner string
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT pg_catalog.pg_get_userbyid(c.relowner)
			FROM pg_catalog.pg_class c
			WHERE c.oid OPERATOR(pg_catalog.=) $1::pg_catalog.regclass`,
			pgx.Identifier{f.schema, "accounts"}.Sanitize()).Scan(&owner))
		for _, g := range built.Fidelity().Grants {
			assert.False(t, g.Public, "the source's owner-default ACL has no PUBLIC entry: %+v", g)
		}
		assert.Contains(t, built.Fidelity().Grants, schemachange.Grant{Privilege: "SELECT", Grantee: owner})
		assert.Contains(t, built.Fidelity().Grants, schemachange.Grant{Privilege: "UPDATE", Grantee: owner})
	})

	// A policy TO PUBLIC is stored as the pseudo-role OID 0, found with
	// 0 = ANY (polroles). The catalog reaches oid = oid through a cast of the
	// literal; an impostor integer = oid matches exactly and would answer
	// false, so the policy would be recorded as applying to no one and the
	// shadow's CREATE POLICY would be refused.
	t.Run("policy applies to PUBLIC", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `
			CREATE FUNCTION %s.never(integer, oid) RETURNS boolean
			LANGUAGE sql AS $$ SELECT false $$`)
		f.exec(t, `CREATE OPERATOR %s.= (LEFTARG = integer, RIGHTARG = oid, FUNCTION = %s.never)`)
		requireImpostorAnswersOnBuildPath(t, f, `SELECT (0 = ANY ('{0}'::oid[]))::text`, "false")

		built := f.buildOpenInsertPolicy(t)

		assert.Equal(t, []schemachange.Policy{openInsertPolicy()}, built.Fidelity().Policies)
	})

	// The role list drops the PUBLIC pseudo-role with r <> 0. An impostor
	// oid <> integer that is always true would keep OID 0 in the list, which
	// prints as a role named "unknown (OID=0)" that does not exist.
	t.Run("policy role filter", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `
			CREATE FUNCTION %s.always_true(oid, integer) RETURNS boolean
			LANGUAGE sql AS $$ SELECT true $$`)
		f.exec(t, `CREATE OPERATOR %s.<> (LEFTARG = oid, RIGHTARG = integer, FUNCTION = %s.always_true)`)
		requireImpostorAnswersOnBuildPath(t, f, `SELECT (0::oid <> 0)::text`, "true")

		built := f.buildOpenInsertPolicy(t)

		assert.Equal(t, []schemachange.Policy{openInsertPolicy()}, built.Fidelity().Policies)
	})

	// The policy's roles come from unnest(polroles). The catalog's unnest takes
	// the polymorphic anyarray, so an impostor unnest(oid[]) is the closer
	// match and would substitute another role for the one the policy names.
	t.Run("policy roles", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %s.accounts (
				id bigint PRIMARY KEY,
				balance numeric
			)`)
		f.exec(t, `ALTER TABLE %s.accounts ENABLE ROW LEVEL SECURITY`)
		f.exec(t, `CREATE POLICY reader_select ON %s.accounts FOR SELECT TO `+pgx.Identifier{reader}.Sanitize()+` USING (true)`)
		f.exec(t, `
			CREATE FUNCTION %s.unnest(oid[]) RETURNS SETOF oid
			LANGUAGE sql AS $$ SELECT `+strconv.FormatUint(uint64(f.roleOID(t, other)), 10)+`::oid $$`)
		requireImpostorAnswersOnBuildPath(t, f,
			`SELECT ARRAY(SELECT r FROM unnest('{1}'::oid[]) r)::text`,
			"{"+strconv.FormatUint(uint64(f.roleOID(t, other)), 10)+"}")

		built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ADD COLUMN note text`)
		require.NoError(t, err)

		assert.Equal(t, []schemachange.Policy{
			{Name: "reader_select", Permissive: true, Command: "r", Roles: []string{reader}, Using: "true"},
		}, built.Fidelity().Policies)
	})
}

// requireImpostorAnswersOnBuildPath runs probe, with %s standing for the
// fixture schema, on the transaction path BuildShadow uses (the target schema
// then public, pg_catalog implicitly first) and requires the planted answer.
// It proves the impostor is reachable on that path, so a clean build means
// the builder's own queries avoided it rather than that it never applied.
func requireImpostorAnswersOnBuildPath(t *testing.T, f shadowFixture, probe, want string) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(t.Context())) }()
	_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(f.schema, "public"))
	require.NoError(t, err)
	var got string
	require.NoError(t, tx.QueryRow(t.Context(), strings.ReplaceAll(probe, "%s", f.schema)).Scan(&got))
	require.Equal(t, want, got, "the impostor must answer the unqualified call on the build path")
}

// comment reads a table's comment by its qualified name, qualifying every
// catalog name so the fixture's own impostors cannot answer.
func (f shadowFixture) comment(t *testing.T, table string) string {
	t.Helper()
	var comment string
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT COALESCE(pg_catalog.obj_description($1::pg_catalog.regclass, 'pg_class'), '')`,
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&comment))
	return comment
}

// roleOID returns the OID of a role.
func (f shadowFixture) roleOID(t *testing.T, role string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT oid FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) $1`, role).Scan(&oid))
	return oid
}

// openInsertPolicy is the policy buildOpenInsertPolicy creates, as the
// snapshot records it.
func openInsertPolicy() schemachange.Policy {
	return schemachange.Policy{
		Name: "open_insert", Permissive: true, Command: "a",
		Roles: []string{}, AppliesToPublic: true, WithCheck: "true",
	}
}

// buildOpenInsertPolicy builds a shadow for a table whose only policy admits
// inserts from PUBLIC.
func (f shadowFixture) buildOpenInsertPolicy(t *testing.T) schemachange.BuiltShadow {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.accounts (
			id bigint PRIMARY KEY,
			balance numeric
		)`)
	f.exec(t, `ALTER TABLE %s.accounts ENABLE ROW LEVEL SECURITY`)
	f.exec(t, `CREATE POLICY open_insert ON %s.accounts FOR INSERT TO PUBLIC WITH CHECK (true)`)
	built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ADD COLUMN note text`)
	require.NoError(t, err)
	return built
}
