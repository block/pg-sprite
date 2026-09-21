package schemachange_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// shadowFixture is a throwaway schema on a superuser pool. The superuser is
// a SET-usable member of every role, so the copy-and-swap proof is minted
// without provisioning and the tests isolate the builder.
type shadowFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	schema string
}

func newShadowFixture(t *testing.T) shadowFixture {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return shadowFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// newShadowFixtureWithRole is newShadowFixture plus one throwaway NOLOGIN
// role, created before the schema so that a relation the test leaves in
// the role's ownership is dropped with the schema before the role is.
func newShadowFixtureWithRole(t *testing.T) (shadowFixture, string) {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	role := testutil.NewRole(t, pool, "NOLOGIN")
	return shadowFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}, role
}

// lock acquires the per-table lock every shadow operation requires and
// releases it when the test ends.
func (f shadowFixture) lock(t *testing.T, table string, options ...dbconn.TableLockOption) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table, options...)
	require.NoError(t, err)
	t.Cleanup(func() {
		// A test that deliberately loses the lock has already seen Release's
		// invariant error through Err; a clean test releases cleanly.
		if lock.Err() == nil {
			assert.NoError(t, lock.Release(context.WithoutCancel(t.Context())))
		}
	})
	return lock
}

// exec runs DDL with %s standing for the fixture schema.
func (f shadowFixture) exec(t *testing.T, ddl string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(ddl, "%s", f.schema))
	require.NoError(t, err)
}

// prove mints the copy-and-swap proof for table.
func (f shadowFixture) prove(t *testing.T, table string) preflight.CopySwapTarget {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, table, preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, table, role)
	require.NoError(t, err)
	return target
}

// alter parses one ALTER TABLE with %s standing for the fixture schema.
func (f shadowFixture) alter(t *testing.T, sql string) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne(strings.ReplaceAll(sql, "%s", f.schema))
	require.NoError(t, err)
	return st
}

// build runs the shadow builder for table with the given ALTER TABLE.
func (f shadowFixture) build(t *testing.T, table, alter string) (schemachange.BuiltShadow, error) {
	t.Helper()
	return schemachange.BuildShadow(t.Context(), f.pool, f.lock(t, table), f.prove(t, table), f.alter(t, alter), schemachange.Options{})
}

// relationExists reports whether any relation wears name in the schema.
func (f shadowFixture) relationExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2)`, f.schema, name).Scan(&exists))
	return exists
}

// columnType reads the canonical type of one column.
func (f shadowFixture) columnType(t *testing.T, table, column string) string {
	t.Helper()
	var typ string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3`, f.schema, table, column).Scan(&typ))
	return typ
}

// columnDefault reads a column's default expression and identity code.
func (f shadowFixture) columnDefault(t *testing.T, table, column string) (def, identity string) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT COALESCE(pg_get_expr(d.adbin, d.adrelid), ''), a.attidentity::text
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3`, f.schema, table, column).Scan(&def, &identity))
	return def, identity
}

// relOptions reads a table's storage parameters and its TOAST relation's,
// the latter prefixed "toast.", as the catalog stores them.
func (f shadowFixture) relOptions(t *testing.T, table string) []string {
	t.Helper()
	var options []string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT COALESCE(c.reloptions, '{}') || ARRAY(SELECT 'toast.' || o FROM pg_class tc CROSS JOIN unnest(tc.reloptions) o WHERE tc.oid = c.reltoastrelid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).Scan(&options))
	return options
}

// aclByOID renders a table's ACL and column ACLs by grantee OID rather than
// name, so two tables compare equal only when their privileges land on the
// same roles.
func (f shadowFixture) aclByOID(t *testing.T, table string) (tableACL, columnACL string) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT COALESCE((SELECT string_agg(a.privilege_type || '->' || a.grantee::text || CASE WHEN a.is_grantable THEN '*' ELSE '' END, ',' ORDER BY 1)
		                 FROM aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a), ''),
		       COALESCE((SELECT string_agg(col.attname || ':' || acl.privilege_type || '->' || acl.grantee::text || CASE WHEN acl.is_grantable THEN '*' ELSE '' END, ',' ORDER BY 1)
		                 FROM pg_attribute col CROSS JOIN LATERAL aclexplode(col.attacl) acl
		                 WHERE col.attrelid = c.oid), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).Scan(&tableACL, &columnACL))
	return tableACL, columnACL
}

// ALTER COLUMN … TYPE bigint on an identity-keyed table: the shadow carries
// the widened column while the source keeps its own; the identity becomes a
// plain DEFAULT drawing from the source's sequence, so a row inserted into
// either table takes the next value of one shared counter; the generated
// column is left out of the copy list because the server computes it.
func TestBuildShadowWidensColumnOnIdentityKeyedTable(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			qty integer NOT NULL,
			note text,
			price integer NOT NULL,
			doubled integer GENERATED ALWAYS AS (price * 2) STORED
		) WITH (fillfactor = 70, toast.autovacuum_enabled = false)`)
	f.exec(t, `COMMENT ON TABLE %s.orders IS 'customer ''orders'''`)
	f.exec(t, `CREATE INDEX orders_note_idx ON %s.orders (note)`)

	built, err := f.build(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint`)
	require.NoError(t, err)

	shadow := schemachange.ShadowName(f.schema, "orders")
	assert.Equal(t, f.schema, built.Schema())
	assert.Equal(t, "orders", built.SourceTable())
	assert.Equal(t, shadow, built.ShadowTable())
	assert.Equal(t, "bigint", f.columnType(t, shadow, "qty"), "the change landed on the shadow")
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "the source is untouched")

	def, identity := f.columnDefault(t, shadow, "id")
	assert.Equal(t, fmt.Sprintf("nextval('%s.orders_id_seq'::regclass)", f.schema), def)
	assert.Equal(t, "", identity, "the shadow's key is a plain column with a default, not an identity")
	assert.Equal(t, []schemachange.IdentityColumn{{
		Column: "id", Always: true, SequenceSchema: f.schema, SequenceName: "orders_id_seq",
		Options: schemachange.SequenceOptions{Start: 1, Increment: 1, Min: 1, Max: 9223372036854775807, Cache: 1},
	}}, built.IdentityColumns())

	assert.Equal(t, []string{"id", "qty", "note", "price"}, built.CopyColumns())
	assert.Equal(t, "customer 'orders'", built.Fidelity().Comment)
	assert.Equal(t, []string{"fillfactor=70", "toast.autovacuum_enabled=false"}, built.Fidelity().RelOptions)
	assert.Equal(t, []string{"fillfactor=70", "toast.autovacuum_enabled=false"}, f.relOptions(t, shadow),
		"LIKE does not carry storage parameters; the builder sets them on the shadow and its TOAST relation")
	assert.True(t, strings.HasPrefix(built.SourceFingerprint(), "sha256:"))
	assert.NotEqual(t, built.SourceFingerprint(), built.TargetFingerprint(), "the widened column changes the shape")

	var comment string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT obj_description(to_regclass($1), 'pg_class')`,
		pgx.Identifier{f.schema, shadow}.Sanitize()).Scan(&comment))
	assert.Equal(t, "customer 'orders'", comment)

	var sourceID, shadowID int64
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`INSERT INTO %s.orders (qty, price) VALUES (1, 10) RETURNING id`, f.schema)).Scan(&sourceID))
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`INSERT INTO %s (qty, price) VALUES (2, 20) RETURNING id`, pgx.Identifier{f.schema, shadow}.Sanitize())).Scan(&shadowID))
	assert.Equal(t, int64(1), sourceID)
	assert.Equal(t, int64(2), shadowID, "source and shadow draw from one sequence")
}

// A serial key already is a plain column defaulting to nextval; LIKE
// INCLUDING DEFAULTS copies that default verbatim, so the shadow shares the
// sequence with no identity handoff to record.
func TestBuildShadowSharesSerialSequence(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.events (
			id serial PRIMARY KEY,
			payload text
		)`)

	built, err := f.build(t, "events", `ALTER TABLE %s.events ADD COLUMN kind text`)
	require.NoError(t, err)

	def, _ := f.columnDefault(t, built.ShadowTable(), "id")
	assert.Equal(t, fmt.Sprintf("nextval('%s.events_id_seq'::regclass)", f.schema), def)
	assert.Empty(t, built.IdentityColumns())
	assert.Equal(t, []string{"id", "payload"}, built.CopyColumns(), "the added column has no source counterpart to copy")
}

// tableSecurity is the security-relevant metadata LIKE does not carry, read
// back from the catalog for one table.
type tableSecurity struct {
	owner           string
	replicaIdentity string
	rlsEnabled      bool
	rlsForced       bool
	grants          []string
	columnGrants    []string
	policies        []string
}

func (f shadowFixture) security(t *testing.T, table string) tableSecurity {
	t.Helper()
	var s tableSecurity
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT pg_get_userbyid(c.relowner), c.relreplident::text, c.relrowsecurity, c.relforcerowsecurity,
		       ARRAY(SELECT a.privilege_type || ' -> ' || CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || CASE WHEN a.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
		             FROM aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a ORDER BY 1),
		       ARRAY(SELECT col.attname || ': ' || acl.privilege_type || ' -> ' || CASE WHEN acl.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(acl.grantee) END || CASE WHEN acl.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
		             FROM pg_attribute col CROSS JOIN LATERAL aclexplode(col.attacl) acl WHERE col.attrelid = c.oid ORDER BY 1),
		       ARRAY(SELECT p.polname || ' ' || CASE WHEN p.polpermissive THEN 'PERMISSIVE' ELSE 'RESTRICTIVE' END
		                    || ' ' || p.polcmd::text || ' ' || array_to_string(ARRAY(SELECT CASE WHEN r = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(r) END FROM unnest(p.polroles) r), ',')
		                    || ' USING ' || COALESCE(pg_get_expr(p.polqual, p.polrelid), '-')
		                    || ' CHECK ' || COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '-')
		             FROM pg_policy p WHERE p.polrelid = c.oid ORDER BY p.polname)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).
		Scan(&s.owner, &s.replicaIdentity, &s.rlsEnabled, &s.rlsForced, &s.grants, &s.columnGrants, &s.policies))
	return s
}

// A table owned by a non-connected role with table grants (one WITH GRANT
// OPTION), a column grant, enabled row-level security, policies (one to a
// role, one to PUBLIC), and REPLICA IDENTITY FULL: the shadow is created
// under SET LOCAL ROLE owner and carries every one of those facts, so the
// ST-5 gate later finds nothing to refuse on. FORCE ROW LEVEL SECURITY is
// absent because the shape gate refuses it.
func TestBuildShadowReplicatesSecurityMetadata(t *testing.T) {
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// Roles before the schema, so the schema (and the grants on it) is
	// dropped before the roles are.
	owner := testutil.NewRole(t, pool, "NOLOGIN")
	reader := testutil.NewRole(t, pool, "NOLOGIN")
	f := shadowFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `GRANT USAGE, CREATE ON SCHEMA %s TO `+pgx.Identifier{owner}.Sanitize())
	f.exec(t, `
		CREATE TABLE %s.accounts (
			id bigint PRIMARY KEY,
			tenant text NOT NULL,
			balance numeric
		)`)
	f.exec(t, `ALTER TABLE %s.accounts OWNER TO `+pgx.Identifier{owner}.Sanitize())
	f.exec(t, `GRANT SELECT ON %s.accounts TO `+pgx.Identifier{reader}.Sanitize()+` WITH GRANT OPTION`)
	f.exec(t, `GRANT INSERT ON %s.accounts TO PUBLIC`)
	f.exec(t, `GRANT UPDATE (balance) ON %s.accounts TO `+pgx.Identifier{reader}.Sanitize())
	f.exec(t, `ALTER TABLE %s.accounts ENABLE ROW LEVEL SECURITY`)
	f.exec(t, `CREATE POLICY tenant_read ON %s.accounts AS PERMISSIVE FOR SELECT TO `+pgx.Identifier{reader}.Sanitize()+` USING (tenant = current_user)`)
	f.exec(t, `CREATE POLICY positive_only ON %s.accounts AS RESTRICTIVE FOR INSERT TO PUBLIC WITH CHECK (balance >= 0)`)
	f.exec(t, `ALTER TABLE %s.accounts REPLICA IDENTITY FULL`)

	built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN balance SET NOT NULL`)
	require.NoError(t, err)

	want := tableSecurity{
		owner:           owner,
		replicaIdentity: "f",
		rlsEnabled:      true,
		rlsForced:       false,
		grants:          f.security(t, "accounts").grants,
		columnGrants:    []string{"balance: UPDATE -> " + reader},
		policies: []string{
			"positive_only RESTRICTIVE a PUBLIC USING - CHECK (balance >= (0)::numeric)",
			"tenant_read PERMISSIVE r " + reader + " USING (tenant = CURRENT_USER) CHECK -",
		},
	}
	assert.Equal(t, want, f.security(t, built.ShadowTable()))
	assert.Contains(t, want.grants, "SELECT -> "+reader+" WITH GRANT OPTION")
	assert.Contains(t, want.grants, "INSERT -> PUBLIC")
	assert.Equal(t, owner, built.Fidelity().Owner)
	assert.Equal(t, "f", built.Fidelity().ReplicaIdentity)
	assert.Equal(t, []schemachange.Policy{
		{Name: "positive_only", Command: "a", Roles: []string{}, AppliesToPublic: true, WithCheck: "(balance >= (0)::numeric)"},
		{Name: "tenant_read", Permissive: true, Command: "r", Roles: []string{reader}, Using: "(tenant = CURRENT_USER)"},
	}, built.Fidelity().Policies)
}

// The owner's default privileges (ALTER DEFAULT PRIVILEGES) land on every
// table the owner creates, including the shadow, but they are not part of
// the source's ACL. The builder synchronises the shadow's ACL to the
// source's, so a default grant to PUBLIC the source never carried is revoked
// before a row is copied into the shadow.
func TestBuildShadowRevokesDefaultPrivilegesTheSourceLacks(t *testing.T) {
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	owner := testutil.NewRole(t, pool, "NOLOGIN")
	f := shadowFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `GRANT USAGE, CREATE ON SCHEMA %s TO `+pgx.Identifier{owner}.Sanitize())
	f.exec(t, `
		CREATE TABLE %s.accounts (
			id bigint PRIMARY KEY,
			balance numeric
		)`)
	f.exec(t, `ALTER TABLE %s.accounts OWNER TO `+pgx.Identifier{owner}.Sanitize())
	f.exec(t, `ALTER DEFAULT PRIVILEGES FOR ROLE `+pgx.Identifier{owner}.Sanitize()+` IN SCHEMA %s GRANT SELECT ON TABLES TO PUBLIC`)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), `ALTER DEFAULT PRIVILEGES FOR ROLE `+pgx.Identifier{owner}.Sanitize()+` IN SCHEMA `+pgx.Identifier{f.schema}.Sanitize()+` REVOKE SELECT ON TABLES FROM PUBLIC`)
		assert.NoError(t, err)
	})

	built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ADD COLUMN note text`)
	require.NoError(t, err)

	sourceACL, _ := f.aclByOID(t, "accounts")
	shadowACL, _ := f.aclByOID(t, built.ShadowTable())
	assert.Equal(t, sourceACL, shadowACL)
	assert.NotContains(t, f.security(t, built.ShadowTable()).grants, "SELECT -> PUBLIC")
}

// "public" is reserved as a role name; "PUBLIC" is not. A grant to a real
// role spelled PUBLIC must land on that role, not on the pseudo-role that
// every role in the database is a member of.
func TestBuildShadowDoesNotWidenAGrantToTheRoleNamedPUBLIC(t *testing.T) {
	if os.Getenv("PG_DSN") != "" {
		t.Skip("creates the cluster-level role \"PUBLIC\"; container-only")
	}
	f := newShadowFixture(t)
	_, err := f.pool.Exec(t.Context(), `CREATE ROLE "PUBLIC"`)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := f.pool.Exec(ctx, `DROP OWNED BY "PUBLIC"`)
		assert.NoError(t, err)
		_, err = f.pool.Exec(ctx, `DROP ROLE "PUBLIC"`)
		assert.NoError(t, err)
	})

	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			secret text
		)`)
	f.exec(t, `GRANT SELECT ON %s.widgets TO "PUBLIC"`)
	f.exec(t, `GRANT UPDATE (secret) ON %s.widgets TO "PUBLIC"`)
	f.exec(t, `CREATE POLICY only_public ON %s.widgets FOR SELECT TO "PUBLIC" USING (true)`)

	built, err := f.build(t, "widgets", `ALTER TABLE %s.widgets ADD COLUMN note text`)
	require.NoError(t, err)

	// Compare grantee OIDs, not the names they print as.
	policyRoles := func(table string) string {
		var roles string
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT p.polroles::text
			FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).Scan(&roles))
		return roles
	}
	sourceTable, sourceColumn := f.aclByOID(t, "widgets")
	shadowTable, shadowColumn := f.aclByOID(t, built.ShadowTable())
	assert.Equal(t, sourceTable, shadowTable, "table grant must land on the same grantee")
	assert.Equal(t, sourceColumn, shadowColumn, "column grant must land on the same grantee")
	assert.Equal(t, policyRoles("widgets"), policyRoles(built.ShadowTable()), "the policy must apply to the same roles")
	assert.NotContains(t, shadowTable, "->0", "nothing was widened to the pseudo-role")
	assert.NotEqual(t, "{0}", policyRoles(built.ShadowTable()))
}

// A table placed in its own tablespace: LIKE creates the shadow in the
// database default, so the builder moves the still-empty shadow to the
// source's tablespace and the snapshot carries it for the cutover gate.
func TestBuildShadowKeepsTheSourceTablespace(t *testing.T) {
	if os.Getenv("PG_DSN") != "" {
		t.Skip("creates a tablespace directory on the server's filesystem; container-only")
	}
	f := newShadowFixture(t)
	tablespace := f.schema + "_ts"
	f.exec(t, `COPY (SELECT 1) TO PROGRAM 'mkdir -p /tmp/`+tablespace+`'`)
	f.exec(t, `CREATE TABLESPACE `+tablespace+` LOCATION '/tmp/`+tablespace+`'`)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := f.pool.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{f.schema}.Sanitize()+` CASCADE`)
		assert.NoError(t, err)
		_, err = f.pool.Exec(ctx, `DROP TABLESPACE `+tablespace)
		assert.NoError(t, err)
	})
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			note text
		) TABLESPACE `+tablespace)

	built, err := f.build(t, "widgets", `ALTER TABLE %s.widgets ADD COLUMN sku text`)
	require.NoError(t, err)

	assert.Equal(t, tablespace, built.Fidelity().Tablespace)
	var shadowTablespace string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT COALESCE((SELECT t.spcname FROM pg_tablespace t WHERE t.oid = c.reltablespace), '<default>')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, f.schema, built.ShadowTable()).Scan(&shadowTablespace))
	assert.Equal(t, tablespace, shadowTablespace)
}

// A DROP COLUMN change leaves the shadow with nowhere to put that column,
// so the copy list the copier builds its INSERT from excludes it while
// keeping every column both tables share.
func TestBuildShadowExcludesDroppedColumnFromCopyColumns(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			keep text,
			gone text
		)`)

	built, err := f.build(t, "widgets", `ALTER TABLE %s.widgets DROP COLUMN gone`)
	require.NoError(t, err)

	assert.Equal(t, []string{"id", "keep"}, built.CopyColumns())
}

// A CHECK constraint the user left NOT VALID would be copied as validated by
// LIKE; the builder re-adds it NOT VALID so the shadow carries the source's
// semantics and the swap cannot silently validate it. A validated sibling
// stays validated.
func TestBuildShadowKeepsCheckConstraintUnvalidated(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.stock (
			id bigint PRIMARY KEY,
			qty integer,
			CONSTRAINT qty_small CHECK (qty < 1000000)
		)`)
	f.exec(t, `INSERT INTO %s.stock VALUES (1, -5)`)
	f.exec(t, `ALTER TABLE %s.stock ADD CONSTRAINT qty_positive CHECK (qty > 0) NOT VALID`)

	built, err := f.build(t, "stock", `ALTER TABLE %s.stock ADD COLUMN note text`)
	require.NoError(t, err)

	assert.Equal(t, []schemachange.UnvalidatedConstraint{{Name: "qty_positive", Def: "CHECK ((qty > 0)) NOT VALID"}}, built.Fidelity().UnvalidatedChecks)
	rows, err := f.pool.Query(t.Context(), `
		SELECT conname, convalidated
		FROM pg_constraint
		WHERE conrelid = to_regclass($1) AND contype = 'c'
		ORDER BY conname`, pgx.Identifier{f.schema, built.ShadowTable()}.Sanitize())
	require.NoError(t, err)
	validated, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var name string
		var valid bool
		err := row.Scan(&name, &valid)
		return fmt.Sprintf("%s=%t", name, valid), err
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"qty_positive=false", "qty_small=true"}, validated)
}

// Dependents wearing the longest names the server accepts — a 63-byte
// index and an identity sequence derived from a 63-byte table name — build
// a shadow whose derived retained names still fit under the identifier
// limit, because the dependent's name is hashed rather than appended.
func TestBuildShadowDerivesRetainedNamesUnderTheIdentifierLimit(t *testing.T) {
	f := newShadowFixture(t)
	table := strings.Repeat("t", 63)
	longIndex := strings.Repeat("i", 63)
	f.exec(t, `
		CREATE TABLE %s.`+table+` (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			sku text
		)`)
	f.exec(t, `CREATE INDEX `+longIndex+` ON %s.`+table+` (sku)`)

	built, err := f.build(t, table, `ALTER TABLE %s.`+table+` ADD COLUMN note text`)
	require.NoError(t, err)

	require.Len(t, built.IdentityColumns(), 1)
	for _, dependent := range []string{longIndex, table + "_pkey", built.IdentityColumns()[0].SequenceName} {
		assert.LessOrEqual(t, len(schemachange.OldDependentName(f.schema, table, dependent)), 63, dependent)
	}
	assert.True(t, f.relationExists(t, schemachange.ShadowName(f.schema, table)))
}

// A relation already wearing the shadow's name is an earlier run's leftover;
// the builder refuses with ErrShadowExists rather than touching it.
func TestBuildShadowRefusesExistingShadow(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE TABLE %s.`+schemachange.ShadowName(f.schema, "widgets")+` (id bigint)`)

	_, err := f.build(t, "widgets", `ALTER TABLE %s.widgets ADD COLUMN note text`)
	require.ErrorIs(t, err, schemachange.ErrShadowExists)
}

// Shape facts are re-read after SET LOCAL ROLE inside the build transaction.
// OID-bound dependents added after proof minting therefore produce their
// typed shape refusal before the shadow is created.
func TestBuildShadowRefusesShapeChangedAfterProof(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			updated_at timestamptz
		)`)
	f.exec(t, `
		CREATE TABLE %s.widget_refs (
			id bigint PRIMARY KEY,
			widget_id bigint
		)`)
	target := f.prove(t, "widgets")
	f.exec(t, `
		CREATE FUNCTION %s.touch_widget() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			NEW.updated_at := now();
			RETURN NEW;
		END $$`)
	f.exec(t, `CREATE TRIGGER touch_row BEFORE UPDATE ON %s.widgets FOR EACH ROW EXECUTE FUNCTION %s.touch_widget()`)
	f.exec(t, `ALTER TABLE %s.widget_refs ADD CONSTRAINT widget_fk FOREIGN KEY (widget_id) REFERENCES %s.widgets (id)`)

	_, err := schemachange.BuildShadow(t.Context(), f.pool, f.lock(t, "widgets"), target,
		f.alter(t, `ALTER TABLE %s.widgets ADD COLUMN note text`), schemachange.Options{})
	var shapeErr *preflight.UnsupportedCopySwapShapeError
	require.ErrorAs(t, err, &shapeErr)
	assert.Equal(t, preflight.CopySwapCauseForeignKeys, shapeErr.Cause)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")))
}

// A statement that does not name the proven table, or is not an ALTER
// TABLE at all, is an invariant violation: the proof for one table can
// never carry DDL against another (ST-7), and nothing is created.
func TestBuildShadowRefusesStatementOutsideProof(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.other (
			id bigint PRIMARY KEY
		)`)
	target := f.prove(t, "widgets")
	lock := f.lock(t, "widgets")

	for name, sql := range map[string]string{
		"other table":  `ALTER TABLE %s.other ADD COLUMN note text`,
		"other schema": `ALTER TABLE public.widgets ADD COLUMN note text`,
		"not ALTER":    `CREATE INDEX widgets_id_idx ON %s.widgets (id)`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, sql), schemachange.Options{})
			require.ErrorIs(t, err, schemachange.ErrInvariantViolation)
		})
	}
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")))
}

// A schema change the server rejects rolls the whole build back: the shadow
// does not survive a failed ALTER, so a retry starts from a clean schema.
func TestBuildShadowLeavesNothingWhenAlterFails(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			note text
		)`)

	_, err := f.build(t, "widgets", `ALTER TABLE %s.widgets ADD COLUMN note text`)
	require.Error(t, err)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")))
}
