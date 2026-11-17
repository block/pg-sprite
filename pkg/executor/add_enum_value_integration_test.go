package executor_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
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
			st := mustParse(t, fmt.Sprintf(tc.statement, schema))
			et, err := preflight.CheckEnumTarget(t.Context(), pool, st.EnumType())
			require.NoError(t, err)
			checked := et.OID()
			var other uint32
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT to_regtype($1)::oid`, schema+".weather").Scan(&other))

			_, err = pool.Exec(t.Context(), fmt.Sprintf(`
				ALTER TYPE %[1]s.mood RENAME TO mood_old;
				ALTER TYPE %[1]s.weather RENAME TO mood`, schema))
			require.NoError(t, err)

			budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second}
			retry := executor.RetryPolicy{MaxAttempts: 1, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}
			err = executor.ExecuteEnumValueAdd(t.Context(), pool, et, st, budget, retry)
			require.ErrorIs(t, err, executor.ErrEnumTypeDrift)
			assert.Equal(t, executor.CodeEnumTypeDrift, executor.OutcomeCode(err))
			assert.Equal(t, tc.checked, labelsOf(t, pool, checked), "the checked type is unchanged")
			assert.Equal(t, tc.other, labelsOf(t, pool, other), "the other type is unchanged")
		})
	}
}

// The name can also move while an attempt is waiting. Another session holds
// the checked type's lock by adding a label of its own, the attempt queues
// behind it, and that session gives the name to a different type before it
// commits. The statement resolved the name before it waited, so a label it
// writes goes to the checked type, which the executor confirms from the row
// it wrote, and the add succeeds. Under IF NOT EXISTS with the label already
// present, nothing is written to identify the type the statement reached,
// and the name no longer resolves to the checked type, so the attempt is
// refused with enum-type-drift and adds nothing to either type.
func TestExecuteEnumValueAddWhileTheNameMovesDuringItsWait(t *testing.T) {
	for _, tc := range []struct {
		name         string
		checked      []string
		other        []string
		statement    string
		refused      bool
		checkedAfter []string
	}{
		{
			name:         "the statement writes the label",
			checked:      []string{"happy", "sad"},
			other:        []string{"sunny"},
			statement:    "ALTER TYPE %s.mood ADD VALUE 'calm'",
			checkedAfter: []string{"happy", "sad", "held", "calm"},
		},
		{
			name:         "IF NOT EXISTS finds the label and writes nothing",
			checked:      []string{"happy", "calm"},
			other:        []string{"sunny"},
			statement:    "ALTER TYPE %s.mood ADD VALUE IF NOT EXISTS 'calm'",
			refused:      true,
			checkedAfter: []string{"happy", "calm", "held"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := newPool(t)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TYPE %[1]s.mood AS ENUM (%[2]s);
				CREATE TYPE %[1]s.weather AS ENUM (%[3]s)`, schema, enumList(tc.checked), enumList(tc.other)))
			require.NoError(t, err)
			st := mustParse(t, fmt.Sprintf(tc.statement, schema))
			et, err := preflight.CheckEnumTarget(t.Context(), pool, st.EnumType())
			require.NoError(t, err)
			var other uint32
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT to_regtype($1)::oid`, schema+".weather").Scan(&other))

			mover, err := pool.Begin(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = mover.Rollback(context.WithoutCancel(t.Context())) })
			_, err = mover.Exec(t.Context(), fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE 'held'", schema))
			require.NoError(t, err)

			waitBudget := executor.Budget{LockTimeout: 20 * time.Second, StatementTimeout: 25 * time.Second}
			oneAttempt := executor.RetryPolicy{MaxAttempts: 1, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}
			done := make(chan error, 1)
			go func() {
				done <- executor.ExecuteEnumValueAdd(t.Context(), pool, et, st, waitBudget, oneAttempt)
			}()
			waitForEnumValueLockWait(t, pool)
			_, err = mover.Exec(t.Context(), fmt.Sprintf(`
				ALTER TYPE %[1]s.mood RENAME TO mood_old;
				ALTER TYPE %[1]s.weather RENAME TO mood`, schema))
			require.NoError(t, err)
			require.NoError(t, mover.Commit(t.Context()))

			select {
			case err := <-done:
				if tc.refused {
					require.ErrorIs(t, err, executor.ErrEnumTypeDrift)
					assert.Equal(t, executor.CodeEnumTypeDrift, executor.OutcomeCode(err))
				} else {
					require.NoError(t, err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the waiting attempt must return once the other session commits")
			}
			assert.Equal(t, tc.checkedAfter, labelsOf(t, pool, et.OID()), "only the checked type can carry a label this attempt added")
			assert.Equal(t, tc.other, labelsOf(t, pool, other), "the type that took the name is unchanged")
		})
	}
}

// waitForEnumValueLockWait waits until a session running an ALTER TYPE ...
// ADD VALUE is waiting on a lock.
func waitForEnumValueLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			  WHERE query LIKE 'ALTER TYPE % ADD VALUE %' AND wait_event_type = 'Lock')`).Scan(&waiting)
		return err == nil && waiting
	}, 30*time.Second, 25*time.Millisecond, "the ADD VALUE must be waiting on the type's lock")
}

// The executor takes only an enum target checked for the name its
// statement spells. A target checked for another type, or one no check
// minted, is an invariant violation before anything runs.
func TestExecuteEnumValueAddRefusesATargetCheckedForAnotherName(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TYPE %[1]s.mood AS ENUM ('happy', 'sad');
		CREATE TYPE %[1]s.weather AS ENUM ('sunny')`, schema))
	require.NoError(t, err)
	weather, err := preflight.CheckEnumTarget(t.Context(), pool, mustParse(t, fmt.Sprintf("ALTER TYPE %s.weather ADD VALUE 'calm'", schema)).EnumType())
	require.NoError(t, err)
	st := mustParse(t, fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE 'calm'", schema))
	budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second}
	retry := executor.RetryPolicy{MaxAttempts: 1, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}

	for name, et := range map[string]preflight.EnumTarget{"another type": weather, "no check": {}} {
		err := executor.ExecuteEnumValueAdd(t.Context(), pool, et, st, budget, retry)
		assert.ErrorIs(t, err, executor.ErrInvariantViolation, name)
	}
	mood, err := preflight.CheckEnumTarget(t.Context(), pool, st.EnumType())
	require.NoError(t, err)
	assert.Equal(t, []string{"happy", "sad"}, labelsOf(t, pool, mood.OID()), "nothing was added")
	assert.Equal(t, []string{"sunny"}, labelsOf(t, pool, weather.OID()), "nothing was added")
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
