package schemachange_test

import (
	"fmt"
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
	pool   *pgxpool.Pool
	schema string
}

func newShadowFixture(t *testing.T) shadowFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return shadowFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
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
	return schemachange.BuildShadow(t.Context(), f.pool, f.prove(t, table), f.alter(t, alter), schemachange.Options{})
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
		) WITH (fillfactor = 70)`)
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
	assert.Equal(t, []string{"fillfactor=70"}, built.Fidelity().RelOptions)
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
	policies        []string
}

func (f shadowFixture) security(t *testing.T, table string) tableSecurity {
	t.Helper()
	var s tableSecurity
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT pg_get_userbyid(c.relowner), c.relreplident::text, c.relrowsecurity, c.relforcerowsecurity,
		       ARRAY(SELECT a.privilege_type || ' -> ' || CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		             FROM aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a ORDER BY 1),
		       ARRAY(SELECT p.polname || ' ' || CASE WHEN p.polpermissive THEN 'PERMISSIVE' ELSE 'RESTRICTIVE' END
		                    || ' ' || p.polcmd::text || ' ' || array_to_string(ARRAY(SELECT CASE WHEN r = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(r) END FROM unnest(p.polroles) r), ',')
		                    || ' USING ' || COALESCE(pg_get_expr(p.polqual, p.polrelid), '-')
		                    || ' CHECK ' || COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '-')
		             FROM pg_policy p WHERE p.polrelid = c.oid ORDER BY p.polname)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).
		Scan(&s.owner, &s.replicaIdentity, &s.rlsEnabled, &s.rlsForced, &s.grants, &s.policies))
	return s
}

// A table owned by a non-connected role with grants, forced row-level
// security, policies (one to a role, one to PUBLIC), and REPLICA IDENTITY
// FULL: the shadow is created under SET ROLE owner and carries every one of
// those facts, so the ST-5 gate later finds nothing to refuse on.
func TestBuildShadowReplicatesSecurityMetadata(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// Roles before the schema, so the schema (and the grants on it) is
	// dropped before the roles are.
	owner := testutil.NewRole(t, pool, "NOLOGIN")
	reader := testutil.NewRole(t, pool, "NOLOGIN")
	f := shadowFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `GRANT USAGE, CREATE ON SCHEMA %s TO `+pgx.Identifier{owner}.Sanitize())
	f.exec(t, `
		CREATE TABLE %s.accounts (
			id bigint PRIMARY KEY,
			tenant text NOT NULL,
			balance numeric
		)`)
	f.exec(t, `ALTER TABLE %s.accounts OWNER TO `+pgx.Identifier{owner}.Sanitize())
	f.exec(t, `GRANT SELECT ON %s.accounts TO `+pgx.Identifier{reader}.Sanitize())
	f.exec(t, `GRANT INSERT ON %s.accounts TO PUBLIC`)
	f.exec(t, `ALTER TABLE %s.accounts ENABLE ROW LEVEL SECURITY`)
	f.exec(t, `ALTER TABLE %s.accounts FORCE ROW LEVEL SECURITY`)
	f.exec(t, `CREATE POLICY tenant_read ON %s.accounts AS PERMISSIVE FOR SELECT TO `+pgx.Identifier{reader}.Sanitize()+` USING (tenant = current_user)`)
	f.exec(t, `CREATE POLICY positive_only ON %s.accounts AS RESTRICTIVE FOR INSERT TO PUBLIC WITH CHECK (balance >= 0)`)
	f.exec(t, `ALTER TABLE %s.accounts REPLICA IDENTITY FULL`)

	built, err := f.build(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN balance SET NOT NULL`)
	require.NoError(t, err)

	want := tableSecurity{
		owner:           owner,
		replicaIdentity: "f",
		rlsEnabled:      true,
		rlsForced:       true,
		grants:          f.security(t, "accounts").grants,
		policies: []string{
			"positive_only RESTRICTIVE a PUBLIC USING - CHECK (balance >= (0)::numeric)",
			"tenant_read PERMISSIVE r " + reader + " USING (tenant = CURRENT_USER) CHECK -",
		},
	}
	assert.Equal(t, want, f.security(t, built.ShadowTable()))
	assert.Contains(t, want.grants, "SELECT -> "+reader)
	assert.Contains(t, want.grants, "INSERT -> PUBLIC")
	assert.Equal(t, owner, built.Fidelity().Owner)
	assert.Equal(t, "f", built.Fidelity().ReplicaIdentity)
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

// An index whose retained _old name would exceed 63 bytes is refused before
// anything is written: cutover could not rename it without truncation.
func TestBuildShadowRefusesLongDependentName(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY,
			sku text
		)`)
	longIndex := strings.Repeat("i", 45)
	f.exec(t, `CREATE INDEX `+longIndex+` ON %s.widgets (sku)`)

	_, err := f.build(t, "widgets", `ALTER TABLE %s.widgets ADD COLUMN note text`)
	require.ErrorIs(t, err, schemachange.ErrNameTooLong)
	assert.Contains(t, err.Error(), schemachange.OldDependentName(f.schema, "widgets", longIndex))
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")))
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

	for name, sql := range map[string]string{
		"other table":  `ALTER TABLE %s.other ADD COLUMN note text`,
		"other schema": `ALTER TABLE public.widgets ADD COLUMN note text`,
		"not ALTER":    `CREATE INDEX widgets_id_idx ON %s.widgets (id)`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schemachange.BuildShadow(t.Context(), f.pool, target, f.alter(t, sql), schemachange.Options{})
			require.ErrorIs(t, err, schemachange.ErrInvariantViolation)
		})
	}
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "widgets")))
}

// The zero proof is refused before any database round trip.
func TestBuildShadowRefusesZeroProof(t *testing.T) {
	f := newShadowFixture(t)
	_, err := schemachange.BuildShadow(t.Context(), f.pool, preflight.CopySwapTarget{},
		f.alter(t, `ALTER TABLE widgets ADD COLUMN note text`), schemachange.Options{})
	require.ErrorIs(t, err, schemachange.ErrInvariantViolation)
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
