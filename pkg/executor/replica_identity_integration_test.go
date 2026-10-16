package executor_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// A native drop is checked against the replica identity again under its own
// lock, so an identity that moves onto the dropped object after planning
// still stops the drop: the column, the constraint, or a child's index stays
// in place, and the outcome is the permanent replica-identity-drop code. A
// drop the identity does not depend on runs as before.
func TestExecuteNativeRefusesADropTheReplicaIdentityMovedOnto(t *testing.T) {
	pool, schema := newPool(t)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, ref text NOT NULL, code text NOT NULL, note text)",
		"CREATE UNIQUE INDEX t_ref_idx ON %[1]s.t (ref)",
		"ALTER TABLE %[1]s.t ADD CONSTRAINT t_code_key UNIQUE (code)",
		"CREATE TABLE %[1]s.parent (id bigint, ref text NOT NULL)",
		"CREATE TABLE %[1]s.child () INHERITS (%[1]s.parent)",
		"CREATE UNIQUE INDEX child_ref_idx ON %[1]s.child (ref)",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	tPreflight := mustPreflight(t, pool, schema, "t")
	parentPreflight := mustPreflight(t, pool, schema, "parent")

	// The identity moves after the statements were planned.
	identity := func(table, index string) {
		t.Helper()
		_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.%s REPLICA IDENTITY USING INDEX %s", schema, table, index))
		require.NoError(t, err)
	}

	identity("t", "t_ref_idx")
	err := executor.ExecuteNative(t.Context(), pool, tPreflight, mustParse(t, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN ref", schema)), budget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrReplicaIdentityDrop)
	assert.Equal(t, executor.CodeReplicaIdentityDrop, executor.OutcomeCode(err))
	assert.Contains(t, err.Error(), "DROP COLUMN ref on")
	assert.Equal(t, "text", columnType(t, pool, schema, "t", "ref"))

	require.NoError(t, executor.ExecuteNative(t.Context(), pool, tPreflight,
		mustParse(t, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema)), budget, executor.DefaultRetryPolicy()))

	identity("t", "t_code_key")
	err = executor.ExecuteNative(t.Context(), pool, tPreflight, mustParse(t, fmt.Sprintf("ALTER TABLE %s.t DROP CONSTRAINT t_code_key", schema)), budget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrReplicaIdentityDrop)
	var constraints int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = 't_code_key'", schema+".t").Scan(&constraints))
	assert.Equal(t, 1, constraints)

	identity("child", "child_ref_idx")
	err = executor.ExecuteNative(t.Context(), pool, parentPreflight, mustParse(t, fmt.Sprintf("ALTER TABLE %s.parent DROP COLUMN ref", schema)), budget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrReplicaIdentityDrop)
	assert.Equal(t, "text", columnType(t, pool, schema, "child", "ref"))
}
