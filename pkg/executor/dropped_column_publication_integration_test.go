package executor_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A publication that starts naming a column in its column list or row
// filter after the plan reviewed the column's drop stops the drop with a
// typed dropped-object-drift naming the publication, read under the
// table's lock, and the column survives. A publication of the whole table
// names no column and does not stop it. Column lists and row filters exist
// from PostgreSQL 15; before that only the whole-table case runs.
func TestRunDroppingRefusesAColumnAPublicationNowNames(t *testing.T) {
	pool, _ := newPool(t)
	var version int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version))
	const publicationFiltersSince = 150000

	publication := func(t *testing.T, name, sql string) {
		t.Helper()
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE PUBLICATION %s %s", name, sql))
		require.NoError(t, err)
		t.Cleanup(func() {
			if _, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP PUBLICATION IF EXISTS "+name); err != nil {
				t.Logf("drop publication %s: %v", name, err)
			}
		})
	}
	// runDropping preflights a table named t, so each case has a schema of
	// its own.
	table := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, note text)", schema))
		require.NoError(t, err)
		return schema
	}

	t.Run("whole table", func(t *testing.T) {
		schema := table(t)
		d := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note")
		publication(t, schema+"_whole", fmt.Sprintf("FOR TABLE %s.t", schema))

		require.NoError(t, runDropping(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema)))
		assert.False(t, columnExists(t, pool, schema, "t", "note"))
	})
	if version < publicationFiltersSince {
		return
	}
	for _, tc := range []struct {
		name   string
		suffix string
		pub    string
	}{
		{name: "column list", suffix: "listed", pub: "FOR TABLE %s.t (id, note)"},
		{name: "row filter", suffix: "filtered", pub: "FOR TABLE %s.t WHERE (note <> '')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := table(t)
			d := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note")
			name := schema + "_" + tc.suffix
			publication(t, name, fmt.Sprintf(tc.pub, schema))

			err := runDropping(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema))
			require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
			assert.Equal(t, executor.CodeDroppedObjectDrift, executor.OutcomeCode(err))
			assert.Contains(t, err.Error(), fmt.Sprintf("publication %q now names column note", name))
			assert.Contains(t, err.Error(), "which the plan did not review")
			assert.True(t, columnExists(t, pool, schema, "t", "note"), "the column must survive a refused drop")
		})
	}
}
