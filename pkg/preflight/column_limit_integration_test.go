package preflight_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// A table's column numbers left counts its dropped columns as used, and an
// inheritance child that has used more of its own numbers decides for the
// parent, since an ADD COLUMN on the parent reaches the child too.
func TestColumnNumbersLeftCountsDroppedColumnsAndChildren(t *testing.T) {
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

	left, err := preflight.ColumnNumbersLeft(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Equal(t, preflight.MaxColumnNumber-2, left, "the dropped kind keeps number 2")

	left, err = preflight.ColumnNumbersLeft(t.Context(), pool, schema, "parent")
	require.NoError(t, err)
	assert.Equal(t, preflight.MaxColumnNumber-12, left, "the child has used 12 numbers: id, note, and ten dropped columns")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DO $$ BEGIN
		FOR i IN 1..1598 LOOP
			EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events ADD COLUMN c%%s int', i);
			EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events DROP COLUMN c%%s', i);
		END LOOP;
	END $$`, schema))
	require.NoError(t, err)
	left, err = preflight.ColumnNumbersLeft(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Zero(t, left)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.events ADD COLUMN extra int`, schema))
	require.ErrorContains(t, err, "SQLSTATE 54011", "zero left means PostgreSQL refuses the add")
}
