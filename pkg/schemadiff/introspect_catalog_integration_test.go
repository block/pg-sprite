package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// Introspection runs with the target schema, then public, on search_path.
// PostgreSQL searches pg_catalog first, so a user function or operator with the
// same argument list as a catalog one never wins, but one whose argument list
// fits a call better than the catalog's does: the catalog's takes the call's
// arguments through an implicit cast, a polymorphic parameter, or an untyped
// NULL or literal, and the user's takes them as they are. Each subtest plants
// such an impostor in the target schema, proves it answers the unqualified call
// on introspection's own search_path, and requires introspection to report the
// model the real catalog gives. An impostor answers wrongly without erroring, so
// a read that left its name unqualified would report a wrong model silently.
func TestIntrospectResistsImpostorsInTheTargetSchema(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// newFixture creates the real objects first, then the impostors, so the
	// fixture DDL never runs with an impostor in reach.
	newFixture := func(t *testing.T, objects, impostors string) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(objects, schema))
		require.NoError(t, err, "create the real objects")
		_, err = pool.Exec(t.Context(), fmt.Sprintf(impostors, schema))
		require.NoError(t, err, "plant the impostors")
		return schema
	}
	// requireImpostorAnswers runs probe, an unqualified call, under the
	// search_path introspection sets, and requires the impostor's answer.
	requireImpostorAnswers := func(t *testing.T, schema, probe, want string) {
		t.Helper()
		var got string
		err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), dbconn.LocalSearchPath(schema, "public")); err != nil {
				return err
			}
			return tx.QueryRow(t.Context(), probe).Scan(&got)
		})
		require.NoError(t, err, probe)
		require.Equal(t, want, got, "the impostor must answer the unqualified call on introspection's search_path: "+probe)
	}
	introspect := func(t *testing.T, schema, table string) schemadiff.Model {
		t.Helper()
		m, err := schemadiff.Introspect(t.Context(), pool, schema, table)
		require.NoError(t, err)
		return m
	}

	// reloptions is text[]; the catalog's unnest takes it through a polymorphic
	// parameter, so an unnest(text[]) beats it.
	t.Run("unnest of the storage parameters", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.t (id bigint) WITH (fillfactor = 70)`, `
			CREATE FUNCTION %[1]s.unnest(text[]) RETURNS SETOF text
				LANGUAGE sql IMMUTABLE AS 'SELECT NULL::text WHERE false'`)
		requireImpostorAnswers(t, schema, `SELECT count(*)::text FROM unnest('{fillfactor=70}'::text[])`, "0")

		assert.Equal(t, []string{"fillfactor=70"}, introspect(t, schema, "t").Options)
	})

	// The options are gathered with array_agg over text, which an
	// array_agg(text) aggregate beats the polymorphic catalog one on.
	t.Run("array_agg of the storage parameters", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.t (id bigint) WITH (fillfactor = 70)`, `
			CREATE FUNCTION %[1]s.impostor_step(text[], text) RETURNS text[]
				LANGUAGE sql IMMUTABLE AS 'SELECT ARRAY[''impostor=1'']::text[]';
			CREATE AGGREGATE %[1]s.array_agg(text) (SFUNC = %[1]s.impostor_step, STYPE = text[])`)
		requireImpostorAnswers(t, schema, `SELECT array_agg(v)::text FROM (VALUES ('fillfactor=70'::text)) AS o(v)`, "{impostor=1}")

		assert.Equal(t, []string{"fillfactor=70"}, introspect(t, schema, "t").Options)
	})

	// indkey is read as int2[]; an unnest(int2[]) beats the polymorphic one.
	t.Run("unnest of the replica identity index columns", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.t (id bigint, tenant bigint NOT NULL, ref text NOT NULL);
			CREATE UNIQUE INDEX t_ref_idx ON %[1]s.t (tenant, ref);
			ALTER TABLE %[1]s.t REPLICA IDENTITY USING INDEX t_ref_idx`, `
			CREATE FUNCTION %[1]s.unnest(int2[]) RETURNS SETOF int2
				LANGUAGE sql IMMUTABLE AS 'SELECT NULL::int2 WHERE false'`)
		requireImpostorAnswers(t, schema, `SELECT count(*)::text FROM unnest('{1,2}'::int2[])`, "0")

		assert.Equal(t, schemadiff.ReplicaIdentityKeys{
			Indexes:    []string{"t_ref_idx"},
			Columns:    []string{"ref", "tenant"},
			OwnColumns: []string{"ref", "tenant"},
		}, introspect(t, schema, "t").ReplicaIdentity)
	})

	// pg_partition_ancestors takes a regclass, reached from an oid by an implicit
	// cast, so a pg_partition_ancestors(oid) beats it.
	t.Run("partition ancestors of the replica identity index", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.p (id bigint NOT NULL, ref text NOT NULL) PARTITION BY RANGE (id);
			CREATE TABLE %[1]s.p_1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
			ALTER TABLE %[1]s.p ADD CONSTRAINT p_ref_key UNIQUE (id, ref);
			ALTER TABLE %[1]s.p_1 REPLICA IDENTITY USING INDEX p_1_id_ref_key`, `
			CREATE FUNCTION %[1]s.pg_partition_ancestors(oid) RETURNS TABLE (relid oid)
				LANGUAGE sql IMMUTABLE AS 'SELECT NULL::oid WHERE false'`)
		requireImpostorAnswers(t, schema, `SELECT count(*)::text FROM pg_partition_ancestors(0::oid)`, "0")

		assert.Equal(t, schemadiff.ReplicaIdentityKeys{
			Indexes:     []string{"p_ref_key"},
			Constraints: []string{"p_ref_key"},
			Columns:     []string{"id", "ref"},
		}, introspect(t, schema, "p").ReplicaIdentity)
	})

	// nspname is a name and the '.' beside it is an untyped literal, which an
	// operator on two names takes as it is where the catalog's takes text.
	const nameConcatImpostor = `
		CREATE FUNCTION %[1]s.impostor_concat(name, name) RETURNS text
			LANGUAGE sql IMMUTABLE AS 'SELECT ''impostor''::text';
		CREATE OPERATOR %[1]s.|| (LEFTARG = name, RIGHTARG = name, FUNCTION = %[1]s.impostor_concat)`

	t.Run("name concatenation of the inheritance relations", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.parent (id bigint);
			CREATE TABLE %[1]s.child () INHERITS (%[1]s.parent)`, nameConcatImpostor)
		requireImpostorAnswers(t, schema, `SELECT 'x'::name || '.'`, "impostor")

		assert.Equal(t, []string{schema + ".child"}, introspect(t, schema, "parent").InheritanceChildren)
		assert.Equal(t, []string{schema + ".parent"}, introspect(t, schema, "child").InheritsParents)
	})

	t.Run("name concatenation of the incoming foreign keys", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.customers (id bigint PRIMARY KEY);
			CREATE TABLE %[1]s.orders (
				id bigint PRIMARY KEY,
				customer_id bigint CONSTRAINT orders_customer_fk REFERENCES %[1]s.customers (id)
			)`, nameConcatImpostor)
		requireImpostorAnswers(t, schema, `SELECT 'x'::name || '.'`, "impostor")

		assert.Equal(t, []string{"orders.orders_customer_fk"}, introspect(t, schema, "customers").ReferencedBy)
	})

	// pg_depend's classid is an oid and the relation it is compared with is
	// spelled as a regclass, which the catalog's = reaches with an implicit
	// cast; an operator on (oid, regclass) beats it. Both sides must read the
	// serial default's sequence: the live table and the scratch build of the
	// desired file.
	t.Run("oid and regclass comparison of the sequence dependencies", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.t (id serial PRIMARY KEY, v text)`, `
			CREATE FUNCTION %[1]s.impostor_never(oid, regclass) RETURNS boolean
				LANGUAGE sql IMMUTABLE AS 'SELECT false';
			CREATE OPERATOR %[1]s.= (LEFTARG = oid, RIGHTARG = regclass, FUNCTION = %[1]s.impostor_never)`)
		requireImpostorAnswers(t, schema, `SELECT ('pg_class'::regclass::oid = 'pg_class'::regclass)::text`, "false")

		live := introspect(t, schema, "t")
		assert.True(t, live.Columns[0].SequenceDefault, "the serial default depends on a sequence")
		assert.True(t, live.Columns[0].SequenceOwned, "the sequence is owned by the column")

		desiredFile, err := statement.ParseDesired("CREATE TABLE t (id serial PRIMARY KEY, v text)")
		require.NoError(t, err)
		desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, desiredFile)
		require.NoError(t, err)
		assert.True(t, desired.Columns[0].SequenceDefault, "the scratch build reads the dependency the same way")
		assert.True(t, desired.Columns[0].SequenceOwned)
	})

	// The collation's schema and name are names; quote_ident takes text, reached
	// from a name by an implicit cast, so a quote_ident(name) beats it.
	t.Run("quote_ident of a column collation", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.t (id bigint, label text COLLATE "C")`, `
			CREATE FUNCTION %[1]s.quote_ident(name) RETURNS text
				LANGUAGE sql IMMUTABLE AS 'SELECT ''impostor''::text'`)
		requireImpostorAnswers(t, schema, `SELECT quote_ident('x'::name)`, "impostor")

		assert.Equal(t, `pg_catalog."C"`, introspect(t, schema, "t").Columns[1].Collation)
	})

	// The owned sequence's type is formatted with an untyped NULL typmod. Both
	// candidates match the oid exactly, and an untyped argument prefers the
	// string category, so a format_type(oid, text) beats the catalog's
	// (oid, integer).
	t.Run("format_type of an owned sequence's type", func(t *testing.T) {
		schema := newFixture(t, `
			CREATE TABLE %[1]s.t (id bigint GENERATED ALWAYS AS IDENTITY (MAXVALUE 5000))`, `
			CREATE FUNCTION %[1]s.format_type(oid, text) RETURNS text
				LANGUAGE sql IMMUTABLE AS 'SELECT ''impostor''::text'`)
		requireImpostorAnswers(t, schema, `SELECT format_type('bigint'::regtype::oid, NULL)`, "impostor")

		assert.Equal(t,
			&schemadiff.SequenceOptions{Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 5000, Cache: 1},
			introspect(t, schema, "t").Columns[0].Sequence)
	})
}
