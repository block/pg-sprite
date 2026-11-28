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
)

// A table's column numbers left counts its dropped columns as used, and an
// inheritance child or partition that has used more of its own numbers
// decides for the parent, since an ADD COLUMN on the parent reaches it too;
// the read names the tables that decide.
func TestReadColumnNumbersCountsDroppedColumnsAndChildren(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (id bigint, kind text);
		ALTER TABLE %[1]s.events DROP COLUMN kind;
		CREATE TABLE %[1]s.parent (id bigint);
		CREATE TABLE %[1]s.child (note text) INHERITS (%[1]s.parent);
		DO $$ BEGIN
			FOR i IN 1..10 LOOP
				EXECUTE pg_catalog.format('ALTER TABLE %[1]s.child ADD COLUMN c%%s int', i);
				EXECUTE pg_catalog.format('ALTER TABLE %[1]s.child DROP COLUMN c%%s', i);
			END LOOP;
		END $$`, schema))
	require.NoError(t, err)

	quoted := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }

	numbers, err := preflight.ReadColumnNumbers(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Equal(t, preflight.ColumnNumbers{
		Left:    preflight.MaxColumnNumber - 2,
		Fullest: []preflight.FullTable{{Name: quoted("events"), Root: true}},
	}, numbers, "the dropped kind keeps number 2")

	numbers, err = preflight.ReadColumnNumbers(t.Context(), pool, schema, "parent")
	require.NoError(t, err)
	assert.Equal(t, preflight.ColumnNumbers{
		Left:    preflight.MaxColumnNumber - 12,
		Fullest: []preflight.FullTable{{Name: quoted("child")}},
	}, numbers, "the child has used 12 numbers: id, note, and ten dropped columns")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.ledger (id int) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.ledger_1 (id int);
		ALTER TABLE %[1]s.ledger_1 ADD COLUMN gone int;
		ALTER TABLE %[1]s.ledger_1 DROP COLUMN gone;
		ALTER TABLE %[1]s.ledger ATTACH PARTITION %[1]s.ledger_1 FOR VALUES FROM (0) TO (100);
		CREATE TABLE %[1]s.ledger_2 PARTITION OF %[1]s.ledger FOR VALUES FROM (100) TO (200)`, schema))
	require.NoError(t, err)
	numbers, err = preflight.ReadColumnNumbers(t.Context(), pool, schema, "ledger")
	require.NoError(t, err)
	assert.Equal(t, preflight.ColumnNumbers{
		Left:    preflight.MaxColumnNumber - 2,
		Fullest: []preflight.FullTable{{Name: quoted("ledger_1"), Partition: true}},
	}, numbers, "the attached partition's dropped column holds number 2")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DO $$ BEGIN
		FOR i IN 1..1598 LOOP
			EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events ADD COLUMN c%%s int', i);
			EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events DROP COLUMN c%%s', i);
		END LOOP;
	END $$`, schema))
	require.NoError(t, err)
	numbers, err = preflight.ReadColumnNumbers(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Zero(t, numbers.Left)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.events ADD COLUMN extra int`, schema))
	require.ErrorContains(t, err, "SQLSTATE 54011", "zero left means PostgreSQL refuses the add")
}
