package preflight_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// TableHasRows reports whether the table, counting its partitions and
// inheritance children, holds a row, for names that need quoting too. A
// read that another session's ACCESS EXCLUSIVE lock blocks returns
// ErrTableRowsLocked at once instead of waiting for the lock.
func TestTableHasRows(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.empty (id bigint);
		CREATE TABLE %[1]s."Customer List" (id bigint);
		INSERT INTO %[1]s."Customer List" VALUES (1);
		CREATE TABLE %[1]s.events (id bigint) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.events_1 PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (10);
		INSERT INTO %[1]s.events VALUES (1);
		CREATE TABLE %[1]s.parent (id bigint);
		CREATE TABLE %[1]s.child () INHERITS (%[1]s.parent);
		INSERT INTO %[1]s.child VALUES (1)`, schema))
	require.NoError(t, err)

	for table, want := range map[string]bool{"empty": false, "Customer List": true, "events": true, "parent": true} {
		rows, err := preflight.TableHasRows(t.Context(), pool, schema, table)
		require.NoError(t, err, table)
		assert.Equal(t, want, rows, table)
	}

	locker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = locker.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = locker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.empty IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	_, err = preflight.TableHasRows(t.Context(), pool, schema, "empty")
	require.ErrorIs(t, err, preflight.ErrTableRowsLocked)
	assert.Contains(t, err.Error(), schema+".empty")
}

// AddColumnFillsNull asks the server whether a column added NOT NULL
// would be NULL in an existing row, so a domain DEFAULT that fills it
// counts and a domain NOT NULL does not change the answer. A domain CHECK
// the row's value fails is not a NULL fill. NullFilledColumns reads the
// table's rows only for a column that would be left NULL, so a column a
// domain DEFAULT fills is planned without opening the table, even under
// another session's ACCESS EXCLUSIVE lock.
func TestNullFilledColumns(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE DOMAIN %[1]s.tier_default AS text DEFAULT 'free';
		CREATE DOMAIN %[1]s.tier_required AS text NOT NULL;
		CREATE DOMAIN %[1]s.tier_checked AS text DEFAULT '' CHECK (VALUE <> '');
		CREATE TABLE %[1]s.customers (id bigint);
		INSERT INTO %[1]s.customers VALUES (1);
		CREATE TABLE %[1]s.prospects (id bigint)`, schema))
	require.NoError(t, err)

	alter := func(table, columns string) string {
		return fmt.Sprintf("ALTER TABLE %s.%s %s", schema, table, columns)
	}
	sql := alter("customers", "ADD COLUMN a text NOT NULL, ADD COLUMN b tier_default NOT NULL, "+
		"ADD COLUMN c tier_required NOT NULL, ADD COLUMN d tier_checked NOT NULL")
	candidates := []string{"a", "b", "c", "d"}
	fills, err := preflight.AddColumnFillsNull(t.Context(), pool, schema, "customers", sql, candidates)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"a": true, "c": true}, fills)

	filled, err := preflight.NullFilledColumns(t.Context(), pool, schema, "customers", sql, candidates)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"a": true, "c": true}, filled)

	filled, err = preflight.NullFilledColumns(t.Context(), pool, schema, "prospects",
		alter("prospects", "ADD COLUMN a text NOT NULL"), []string{"a"})
	require.NoError(t, err)
	assert.Empty(t, filled, "an empty table has no row to leave NULL")

	locker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = locker.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = locker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.customers IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	filled, err = preflight.NullFilledColumns(t.Context(), pool, schema, "customers",
		alter("customers", "ADD COLUMN b tier_default NOT NULL"), []string{"b"})
	require.NoError(t, err, "a column its domain fills never reads the locked table")
	assert.Empty(t, filled)
	_, err = preflight.NullFilledColumns(t.Context(), pool, schema, "customers",
		alter("customers", "ADD COLUMN a text NOT NULL"), []string{"a"})
	require.ErrorIs(t, err, preflight.ErrTableRowsLocked)
}
