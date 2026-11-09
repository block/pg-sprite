package executor_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// The engine resolves the enum type an ADD VALUE names, checks it, and only
// then runs the statement, which resolves the name again. ALTER TYPE ...
// RENAME does not wait for the lock ADD VALUE takes, so another session can
// give the name to a different type in between. The executor confirms by
// identity, before the commit, that the label went to the type it checked,
// so the attempt rolls back with enum-type-drift and neither type changes:
// whether the statement wrote the label to the other type, wrote it there
// while the checked type already carried it, found it on the other type
// under IF NOT EXISTS and wrote nothing, or found it there while the
// checked type carried it too.
func TestExecuteEnumValueAddRefusesANameMovedToAnotherType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checked   []string
		other     []string
		statement string
	}{
		{
			name:      "the statement writes the label to the other type",
			checked:   []string{"happy", "sad"},
			other:     []string{"sunny"},
			statement: "ALTER TYPE %s.mood ADD VALUE 'calm'",
		},
		{
			name:      "the checked type already carries the label",
			checked:   []string{"happy", "calm"},
			other:     []string{"sunny"},
			statement: "ALTER TYPE %s.mood ADD VALUE 'calm'",
		},
		{
			name:      "IF NOT EXISTS finds the label on the other type",
			checked:   []string{"happy", "sad"},
			other:     []string{"sunny", "calm"},
			statement: "ALTER TYPE %s.mood ADD VALUE IF NOT EXISTS 'calm'",
		},
		{
			name:      "IF NOT EXISTS finds the label on both types",
			checked:   []string{"happy", "calm"},
			other:     []string{"sunny", "calm"},
			statement: "ALTER TYPE %s.mood ADD VALUE IF NOT EXISTS 'calm'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := newPool(t)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TYPE %[1]s.mood AS ENUM (%[2]s);
				CREATE TYPE %[1]s.weather AS ENUM (%[3]s)`, schema, enumList(tc.checked), enumList(tc.other)))
			require.NoError(t, err)
			var checked, other uint32
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT to_regtype($1)::oid, to_regtype($2)::oid`,
				schema+".mood", schema+".weather").Scan(&checked, &other))

			_, err = pool.Exec(t.Context(), fmt.Sprintf(`
				ALTER TYPE %[1]s.mood RENAME TO mood_old;
				ALTER TYPE %[1]s.weather RENAME TO mood`, schema))
			require.NoError(t, err)

			budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second}
			retry := executor.RetryPolicy{MaxAttempts: 1, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}
			err = executor.ExecuteEnumValueAdd(t.Context(), pool, mustParse(t, fmt.Sprintf(tc.statement, schema)),
				schema, checked, budget, retry)
			require.ErrorIs(t, err, executor.ErrEnumTypeDrift)
			assert.Equal(t, executor.CodeEnumTypeDrift, executor.OutcomeCode(err))
			assert.Equal(t, tc.checked, labelsOf(t, pool, checked), "the checked type is unchanged")
			assert.Equal(t, tc.other, labelsOf(t, pool, other), "the other type is unchanged")
		})
	}
}

// enumList spells labels as the value list of CREATE TYPE ... AS ENUM.
func enumList(labels []string) string {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = "'" + l + "'"
	}
	return strings.Join(quoted, ", ")
}

// labelsOf returns the labels of the enum type with OID typ in their sort
// order.
func labelsOf(t *testing.T, pool *pgxpool.Pool, typ uint32) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT enumlabel FROM pg_enum WHERE enumtypid = $1 ORDER BY enumsortorder`, typ)
	require.NoError(t, err)
	labels, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return labels
}
