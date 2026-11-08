package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A live table can hold two PRIMARY KEY, UNIQUE, or EXCLUDE constraints
// declared the same way, each added by its own ALTER TABLE. One CREATE TABLE
// that declares both keeps only one of them, the primary key if either is
// one, without an error. Pull refuses to render such a table, and a desired
// file that declares both fails its build rather than planning the dropped
// constraint's live copy away. Both name the constraint the server drops.

func TestRepeatedUniqueConstraintsAreRefused(t *testing.T) {
	pool, schema := mergedConstraintsFixture(t, `
		CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c1 UNIQUE (a);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c2 UNIQUE (a);`)
	requirePullRefuses(t, pool, schema, `constraint "c2" repeats "c1"`)
	requireBuildRefuses(t, pool, schema, `
		CREATE TABLE t (
		  a int NOT NULL,
		  b int NOT NULL,
		  CONSTRAINT c1 UNIQUE (a),
		  CONSTRAINT c2 UNIQUE (a)
		);`, `table "t", constraint "c2"`)
}

func TestUniqueConstraintRepeatingThePrimaryKeyIsRefused(t *testing.T) {
	pool, schema := mergedConstraintsFixture(t, `
		CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c1 PRIMARY KEY (a, b);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c2 UNIQUE (a, b);`)
	requirePullRefuses(t, pool, schema, `constraint "c2" repeats "c1"`)
	requireBuildRefuses(t, pool, schema, `
		CREATE TABLE t (
		  a int NOT NULL,
		  b int NOT NULL,
		  CONSTRAINT c1 PRIMARY KEY (a, b),
		  CONSTRAINT c2 UNIQUE (a, b)
		);`, `table "t", constraint "c2"`)
}

// The server keeps the primary key even when the UNIQUE constraint comes
// first, so the earlier constraint is the one dropped.
func TestPrimaryKeyRepeatingAnEarlierUniqueConstraintIsRefused(t *testing.T) {
	pool, schema := mergedConstraintsFixture(t, `
		CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c1 UNIQUE (a);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c2 PRIMARY KEY (a);`)
	requirePullRefuses(t, pool, schema, `constraint "c1" repeats "c2"`)
	requireBuildRefuses(t, pool, schema, `
		CREATE TABLE t (
		  a int NOT NULL,
		  b int NOT NULL,
		  CONSTRAINT c1 UNIQUE (a),
		  CONSTRAINT c2 PRIMARY KEY (a)
		);`, `table "t", constraint "c1"`)
}

func TestRepeatedExclusionConstraintsAreRefused(t *testing.T) {
	pool, schema := mergedConstraintsFixture(t, `
		CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c1 EXCLUDE USING btree (a WITH =);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c2 EXCLUDE USING btree (a WITH =);`)
	requirePullRefuses(t, pool, schema, `constraint "c2" repeats "c1"`)
	requireBuildRefuses(t, pool, schema, `
		CREATE TABLE t (
		  a int NOT NULL,
		  b int NOT NULL,
		  CONSTRAINT c1 EXCLUDE USING btree (a WITH =),
		  CONSTRAINT c2 EXCLUDE USING btree (a WITH =)
		);`, `table "t", constraint "c2"`)
}

// A named constraint declared inline on its column is merged like a
// table-level one.
func TestNamedInlineConstraintRepeatedByATableConstraintIsRefused(t *testing.T) {
	pool, schema := mergedConstraintsFixture(t, "")
	requireBuildRefuses(t, pool, schema, `
		CREATE TABLE t (
		  a int CONSTRAINT c1 UNIQUE,
		  CONSTRAINT c2 UNIQUE (a)
		);`, `table "t", constraint "c2"`)
}

// When the earlier constraint is unnamed, the server gives it the dropped
// constraint's name, so every name the file states is built. The table
// still holds one constraint fewer than the file declares, and that is what
// the build refuses.
func TestUnnamedConstraintMergedWithANamedOneIsRefused(t *testing.T) {
	pool, schema := mergedConstraintsFixture(t, "")
	requireBuildRefuses(t, pool, schema, `
		CREATE TABLE t (
		  a int UNIQUE,
		  CONSTRAINT c2 UNIQUE (a)
		);`, `table "t" declares 2 primary key, unique, and exclusion constraints, and building it kept 1`)
}

// An unnamed primary key merged with a named UNIQUE constraint becomes a
// primary key carrying the UNIQUE constraint's name.
func TestUnnamedPrimaryKeyMergedWithANamedUniqueConstraintIsRefused(t *testing.T) {
	pool, schema := mergedConstraintsFixture(t, "")
	requireBuildRefuses(t, pool, schema, `
		CREATE TABLE t (
		  id int PRIMARY KEY,
		  CONSTRAINT t_id_key UNIQUE (id)
		);`, `table "t" declares 2 primary key, unique, and exclusion constraints, and building it kept 1`)
}

// Two constraints that differ in any way the server compares are both kept
// by one CREATE TABLE, so their table renders and round-trips to an empty
// plan.
func TestSimilarConstraintsTheServerKeepsApartRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		live       string
		minVersion int
	}{
		{name: "the same columns in another order", live: `
			CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c1 UNIQUE (a, b);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c2 UNIQUE (b, a);`},
		{name: "one deferrable", live: `
			CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c1 UNIQUE (a);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c2 UNIQUE (a) DEFERRABLE;`},
		{name: "one with an INCLUDE list", live: `
			CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c1 UNIQUE (a);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c2 UNIQUE (a) INCLUDE (b);`},
		{name: "a unique constraint and an exclusion constraint", live: `
			CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c1 UNIQUE (a);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c2 EXCLUDE USING btree (a WITH =);`},
		{name: "NULLS NOT DISTINCT beside the primary key", minVersion: 150000, live: `
			CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c1 PRIMARY KEY (a);
			ALTER TABLE %[1]s.t ADD CONSTRAINT c2 UNIQUE NULLS NOT DISTINCT (a);`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, schema := mergedConstraintsFixture(t, "")
			var num int
			require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int").Scan(&num))
			if num < tt.minVersion {
				t.Skipf("needs server_version_num %d; server reports %d", tt.minVersion, num)
			}
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tt.live, schema))
			require.NoError(t, err)
			_, _, changes := roundTrip(t, pool, schema, "t")
			assert.Empty(t, changes, "the server keeps both constraints, so the rendered file must rebuild them")
		})
	}
}

// mergedConstraintsFixture returns a pool and a fresh schema in which live,
// when not empty, has run with the schema substituted for %[1]s.
func mergedConstraintsFixture(t *testing.T, live string) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	if live != "" {
		_, err = pool.Exec(t.Context(), fmt.Sprintf(live, schema))
		require.NoError(t, err)
	}
	return pool, schema
}

// requirePullRefuses asserts that rendering the live table t is refused,
// naming the repeated pair.
func requirePullRefuses(t *testing.T, pool *pgxpool.Pool, schema, want string) {
	t.Helper()
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableMergedConstraints)
	assert.ErrorContains(t, err, want)
}

// requireBuildRefuses asserts that building desired fails because the
// server merged two of its constraints.
func requireBuildRefuses(t *testing.T, pool *pgxpool.Pool, schema, desired, want string) {
	t.Helper()
	ds, err := statement.ParseDesired(desired)
	require.NoError(t, err)
	_, err = schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.ErrorIs(t, err, schemadiff.ErrDesiredConstraintMerged)
	assert.ErrorContains(t, err, want)
}
