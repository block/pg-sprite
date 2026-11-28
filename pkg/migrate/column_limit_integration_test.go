package migrate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Adding a column to a table whose dropped columns have used every column
// number is refused at plan time, since PostgreSQL would refuse it after
// taking the table's exclusive lock; the detail gives the remedy, and the
// table is left without the column. Dropping a column frees no number, so
// a statement that drops one and adds another is refused too.
func TestRunRefusesAColumnAddPastTheColumnLimit(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (id bigint PRIMARY KEY, note text);
		DO $$ BEGIN
			FOR i IN 1..1598 LOOP
				EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events ADD COLUMN c%%s int', i);
				EXECUTE pg_catalog.format('ALTER TABLE %[1]s.events DROP COLUMN c%%s', i);
			END LOOP;
		END $$`, schema))
	require.NoError(t, err)

	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.events ADD COLUMN kind text", schema))
	v, err := migrate.Run(t.Context(), pool, st, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, v.Detail)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Contains(t, v.Detail, "ADD COLUMN kind adds a column to a table that has used all 1600 of PostgreSQL's column numbers")
	assert.True(t, strings.HasSuffix(v.Detail, "; "+planner.ColumnLimitHelp), v.Detail)
	assert.Empty(t, v.ExecutedSQL)
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM pg_catalog.pg_attribute
		WHERE attrelid = pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.events') AND attname = 'kind')`,
		schema).Scan(&exists))
	assert.False(t, exists, "the refused column must not exist")

	facts, err := migrate.LiveFacts(t.Context(), pool, st)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"kind": true}, facts.Classifier.ColumnLimitReached)

	v, err = migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN note, ADD COLUMN kind text", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, v.Detail)
	assert.Contains(t, v.Detail, "ADD COLUMN kind adds a column to a table that has used all")
}
