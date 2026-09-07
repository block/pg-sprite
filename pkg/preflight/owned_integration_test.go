package preflight_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

func TestLookupOwnedRelationNames(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("constraint indexes and sequences", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		qualified := pgx.Identifier{schema, "t"}.Sanitize()
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+qualified+" (id serial PRIMARY KEY, email text UNIQUE, n int GENERATED ALWAYS AS IDENTITY, CONSTRAINT named_uq UNIQUE (n), EXCLUDE USING btree (n WITH =))")
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), "CREATE INDEX t_extra_idx ON "+qualified+" (email)")
		require.NoError(t, err)

		names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
		require.NoError(t, err)
		assert.Equal(t, []string{"named_uq", "t_email_key", "t_n_excl", "t_pkey"}, names.ConstraintIndexes)
		assert.Equal(t, []string{"t_id_seq", "t_n_seq"}, names.Sequences)
		assert.NotContains(t, names.ConstraintIndexes, "t_extra_idx")
	})

	// A foreign key's constraint row points at the referenced table's index,
	// so it must not be reported as a name the referencing table owns; nor
	// may a self-referencing key report the table's own primary key twice.
	t.Run("foreign keys borrow the referenced table's indexes", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		other := testutil.NewSchema(t, pool)
		parent := pgx.Identifier{other, "parent"}.Sanitize()
		qualified := pgx.Identifier{schema, "t"}.Sanitize()
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+parent+" (id int PRIMARY KEY, code text UNIQUE)")
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"CREATE TABLE %s (id serial PRIMARY KEY, parent_id int REFERENCES %s (id), parent_code text REFERENCES %s (code), self_id int REFERENCES %s (id), CHECK (id > 0))",
			qualified, parent, parent, qualified))
		require.NoError(t, err)

		names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
		require.NoError(t, err)
		assert.Equal(t, []string{"t_pkey"}, names.ConstraintIndexes)
		assert.Equal(t, []string{"t_id_seq"}, names.Sequences)
		assert.NotContains(t, names.ConstraintIndexes, "parent_pkey")
		assert.NotContains(t, names.ConstraintIndexes, "parent_code_key")
	})

	// Ownership is the OWNED BY relationship, not use: a sequence a column
	// merely defaults from is not owned, while a sequence adopted after the
	// fact is — even though the server did not choose its name.
	t.Run("sequences owned by columns", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		loose := pgx.Identifier{schema, "loose"}.Sanitize()
		adopted := pgx.Identifier{schema, "adopted"}.Sanitize()
		qualified := pgx.Identifier{schema, "t"}.Sanitize()
		_, err := pool.Exec(t.Context(), "CREATE SEQUENCE "+loose)
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), "CREATE SEQUENCE "+adopted)
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), "CREATE TABLE "+qualified+" (id int DEFAULT nextval('"+loose+"'), d int)")
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), "ALTER SEQUENCE "+adopted+" OWNED BY "+qualified+".d")
		require.NoError(t, err)

		names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
		require.NoError(t, err)
		assert.Equal(t, []string{}, names.ConstraintIndexes)
		assert.Equal(t, []string{"adopted"}, names.Sequences)
		assert.NotContains(t, names.Sequences, "loose")
	})

	t.Run("server suffixes occupied names", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		qSchema := pgx.Identifier{schema}.Sanitize()
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+qSchema+".sq_pkey (x int)")
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), "CREATE INDEX t_pkey ON "+qSchema+".sq_pkey (x)")
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), "CREATE SEQUENCE "+qSchema+".t_id_seq")
		require.NoError(t, err)
		create := "CREATE TABLE " + qSchema + ".t (id serial PRIMARY KEY)"
		_, err = pool.Exec(t.Context(), create)
		require.NoError(t, err)

		names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
		require.NoError(t, err)
		assert.Equal(t, []string{"t_pkey1"}, names.ConstraintIndexes)
		assert.Equal(t, []string{"t_id_seq1"}, names.Sequences)
		claimed, err := statement.ImplicitRelationNames(create)
		require.NoError(t, err)
		assert.Equal(t, []string{"t_id_seq", "t_pkey"}, claimed)
		for _, name := range claimed {
			assert.NotContains(t, names.ConstraintIndexes, name)
			assert.NotContains(t, names.Sequences, name)
		}
	})

	t.Run("no owned relations", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+pgx.Identifier{schema, "t"}.Sanitize()+" (v text)")
		require.NoError(t, err)

		names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
		require.NoError(t, err)
		assert.Equal(t, []string{}, names.ConstraintIndexes)
		assert.Equal(t, []string{}, names.Sequences)
	})

	t.Run("missing table", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "missing")
		assert.ErrorIs(t, err, preflight.ErrTableNotFound)
	})

	t.Run("view at the name", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), "CREATE VIEW "+pgx.Identifier{schema, "t"}.Sanitize()+" AS SELECT 1 AS v")
		require.NoError(t, err)

		_, err = preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
		assert.ErrorIs(t, err, preflight.ErrNotTable)
		assert.NotErrorIs(t, err, preflight.ErrTableNotFound)
	})

	t.Run("same name in another schema", func(t *testing.T) {
		a := testutil.NewSchema(t, pool)
		b := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s (v text)", pgx.Identifier{a, "t"}.Sanitize()))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s (id serial PRIMARY KEY)", pgx.Identifier{b, "t"}.Sanitize()))
		require.NoError(t, err)

		names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, b, "t")
		require.NoError(t, err)
		assert.Equal(t, []string{"t_pkey"}, names.ConstraintIndexes)
		assert.Equal(t, []string{"t_id_seq"}, names.Sequences)
	})

	t.Run("partitioned parent", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+pgx.Identifier{schema, "t"}.Sanitize()+" (id int PRIMARY KEY) PARTITION BY RANGE (id)")
		require.NoError(t, err)

		names, err := preflight.LookupOwnedRelationNames(t.Context(), pool, schema, "t")
		require.NoError(t, err)
		assert.Equal(t, []string{"t_pkey"}, names.ConstraintIndexes)
		assert.Equal(t, []string{}, names.Sequences)
	})

	// An empty schema is refused before any lookup rather than reported as
	// a table that happens not to exist.
	t.Run("empty schema", func(t *testing.T) {
		_, err := preflight.LookupOwnedRelationNames(t.Context(), pool, "", "t")
		assert.Error(t, err)
		assert.NotErrorIs(t, err, preflight.ErrTableNotFound)
	})
}
