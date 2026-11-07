package migrate_test

import (
	"context"
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
	assert.Equal(t, map[string]bool{"tier": true}, facts.Classifier.NullFilledColumns)

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

// A NOT NULL column with no DEFAULT of its own is refused only when the
// server proves a row would hold NULL in it. A domain DEFAULT fills every
// row, so the column runs. Dropping a column and adding it back NOT NULL
// in one statement leaves every row NULL in the new column, so it is
// refused even though the table has a column of that name. ADD COLUMN IF
// NOT EXISTS for a column the table has adds nothing, so it plans without
// reading the table, even while another session holds it locked.
func TestRunJudgesANotNullColumnByWhatTheServerFillsItWith(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE DOMAIN %[1]s.tier_type AS text DEFAULT 'free';
		CREATE TABLE %[1]s.customers (id bigint PRIMARY KEY, email text NOT NULL);
		INSERT INTO %[1]s.customers VALUES (1, 'a@example.com'), (2, 'b@example.com')`, schema))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %[1]s.customers ADD COLUMN tier %[1]s.tier_type NOT NULL", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	var tiers []string
	require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(
		"SELECT pg_catalog.array_agg(DISTINCT tier::text) FROM %s.customers", schema)).Scan(&tiers))
	assert.Equal(t, []string{"free"}, tiers)

	opts := runOptions()
	opts.AllowDestructive = schema + ".customers"
	v, err = migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.customers DROP COLUMN email, ADD COLUMN email text NOT NULL", schema)), opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, v.Detail)
	assert.Contains(t, v.Detail, "ADD COLUMN email adds a NOT NULL column with no default to a table that holds rows")
	assert.Empty(t, v.ExecutedSQL)

	locker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = locker.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = locker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.customers IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	facts, err := migrate.LiveFacts(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.customers ADD COLUMN IF NOT EXISTS email text NOT NULL", schema)))
	require.NoError(t, err, "a column the table already has is never probed")
	assert.Empty(t, facts.Classifier.NullFilledColumns)
}
