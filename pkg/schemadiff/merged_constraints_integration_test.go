package schemadiff_test

import (
	"fmt"
	"testing"

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
// constraint's live copy away. Two constraints that differ in any way the
// server compares are both kept, so their table renders and round-trips.
func TestRepeatedIndexConstraintsAreRefusedNotDropped(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	var num int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int").Scan(&num))

	tests := []struct {
		name          string
		first, second string
		// dropped names the constraint one CREATE TABLE leaves out, or is
		// empty when the server keeps both.
		dropped    string
		minVersion int
	}{
		{name: "two identical unique constraints", first: "UNIQUE (a)", second: "UNIQUE (a)", dropped: "c2"},
		{name: "a unique constraint repeating the primary key", first: "PRIMARY KEY (a, b)", second: "UNIQUE (a, b)", dropped: "c2"},
		{name: "a primary key repeating an earlier unique constraint", first: "UNIQUE (a)", second: "PRIMARY KEY (a)", dropped: "c1"},
		{name: "two identical exclusion constraints", first: "EXCLUDE USING btree (a WITH =)", second: "EXCLUDE USING btree (a WITH =)", dropped: "c2"},
		{name: "the same columns in another order", first: "UNIQUE (a, b)", second: "UNIQUE (b, a)"},
		{name: "one deferrable", first: "UNIQUE (a)", second: "UNIQUE (a) DEFERRABLE"},
		{name: "one with an INCLUDE list", first: "UNIQUE (a)", second: "UNIQUE (a) INCLUDE (b)"},
		{name: "a unique constraint and an exclusion constraint", first: "UNIQUE (a)", second: "EXCLUDE USING btree (a WITH =)"},
		{name: "NULLS NOT DISTINCT beside the primary key", first: "PRIMARY KEY (a)", second: "UNIQUE NULLS NOT DISTINCT (a)", minVersion: 150000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if num < tt.minVersion {
				t.Skipf("needs server_version_num %d; server reports %d", tt.minVersion, num)
			}
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TABLE %[1]s.t (a int NOT NULL, b int NOT NULL);
				ALTER TABLE %[1]s.t ADD CONSTRAINT c1 %[2]s;
				ALTER TABLE %[1]s.t ADD CONSTRAINT c2 %[3]s;`, schema, tt.first, tt.second))
			require.NoError(t, err)

			if tt.dropped == "" {
				_, _, changes := roundTrip(t, pool, schema, "t")
				assert.Empty(t, changes, "the server keeps both constraints, so the rendered file must rebuild them")
				return
			}

			live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
			require.NoError(t, err)
			_, err = schemadiff.Render(live)
			require.ErrorIs(t, err, schemadiff.ErrUnrenderableMergedConstraints)
			assert.ErrorContains(t, err, `constraint "c2" repeats "c1"`)

			ds, err := statement.ParseDesired(fmt.Sprintf(
				"CREATE TABLE t (a int NOT NULL, b int NOT NULL, CONSTRAINT c1 %s, CONSTRAINT c2 %s);", tt.first, tt.second))
			require.NoError(t, err)
			_, err = schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
			require.ErrorIs(t, err, schemadiff.ErrDesiredConstraintMerged)
			assert.ErrorContains(t, err, fmt.Sprintf(`table "t", constraint %q`, tt.dropped))
		})
	}
}
