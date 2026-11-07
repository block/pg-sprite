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

// Adding a NOT NULL column with no default to a table that holds rows is
// refused at plan time, since PostgreSQL would refuse it after taking the
// table's exclusive lock; the detail gives the manual path, and the table
// is left without the column. The same statement on an empty table runs,
// and so does the column with a constant default on the table with rows.
func TestRunRefusesANotNullColumnWithoutDefaultOnATableWithRows(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.customers (id bigint PRIMARY KEY, email text NOT NULL);
		INSERT INTO %[1]s.customers VALUES (1, 'a@example.com'), (2, 'b@example.com');
		CREATE TABLE %[1]s.prospects (id bigint PRIMARY KEY)`, schema))
	require.NoError(t, err)

	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.customers ADD COLUMN tier text NOT NULL", schema))
	v, err := migrate.Run(t.Context(), pool, st, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, v.Detail)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Contains(t, v.Detail, "ADD COLUMN tier adds a NOT NULL column with no default to a table that holds rows")
	assert.True(t, strings.HasSuffix(v.Detail, "; "+planner.NotNullWithoutDefaultHelp), v.Detail)
	assert.Empty(t, v.ExecutedSQL)
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM pg_catalog.pg_attribute
		WHERE attrelid = pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.customers') AND attname = 'tier')`,
		schema).Scan(&exists))
	assert.False(t, exists, "the refused column must not exist")

	facts, err := migrate.LiveFacts(t.Context(), pool, st)
	require.NoError(t, err)
	assert.True(t, facts.Classifier.TableHasRows)

	v, err = migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.prospects ADD COLUMN tier text NOT NULL", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	assert.Equal(t, "text", columnType(t, pool, schema, "prospects", "tier"))

	v, err = migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.customers ADD COLUMN tier text NOT NULL DEFAULT 'free'", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	assert.Equal(t, "text", columnType(t, pool, schema, "customers", "tier"))
}
