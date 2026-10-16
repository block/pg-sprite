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

// The re-check locks only what the drop reaches. A column dropped under ONLY
// stays in the inheritance children, so a child's identity on it does not
// stop the drop. A primary key or unique constraint is never inherited, so
// dropping one from an inheritance parent neither waits on a busy child nor
// is refused for it. On a partitioned table the partitions' indexes are
// attached under the parent's, so the parent's constraint drop still reaches
// a partition's identity and is refused.
func TestExecuteNativeChecksTheReplicaIdentityOfWhatTheDropReaches(t *testing.T) {
	pool, schema := newPool(t)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.parent (id bigint, ref text NOT NULL, code text NOT NULL)",
		"ALTER TABLE %[1]s.parent ADD CONSTRAINT parent_code_key UNIQUE (code)",
		"CREATE TABLE %[1]s.child () INHERITS (%[1]s.parent)",
		"CREATE UNIQUE INDEX child_ref_idx ON %[1]s.child (ref)",
		"CREATE TABLE %[1]s.p (id bigint NOT NULL, ref text NOT NULL) PARTITION BY RANGE (id)",
		"CREATE TABLE %[1]s.p_1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100)",
		"ALTER TABLE %[1]s.p ADD CONSTRAINT p_ref_key UNIQUE (id, ref)",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	parentPreflight := mustPreflight(t, pool, schema, "parent")
	pPreflight := mustPreflight(t, pool, schema, "p")
	for _, ddl := range []string{
		"ALTER TABLE %[1]s.child REPLICA IDENTITY USING INDEX child_ref_idx",
		"ALTER TABLE %[1]s.p_1 REPLICA IDENTITY USING INDEX p_1_id_ref_key",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}

	require.NoError(t, executor.ExecuteNative(t.Context(), pool, parentPreflight,
		mustParse(t, fmt.Sprintf("ALTER TABLE ONLY %s.parent DROP COLUMN ref", schema)), budget, executor.DefaultRetryPolicy()))
	assert.Equal(t, "text", columnType(t, pool, schema, "child", "ref"))

	busy, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = busy.Rollback(t.Context()) }()
	_, err = busy.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.child IN ACCESS SHARE MODE", schema))
	require.NoError(t, err)
	require.NoError(t, executor.ExecuteNative(t.Context(), pool, parentPreflight,
		mustParse(t, fmt.Sprintf("ALTER TABLE %s.parent DROP CONSTRAINT parent_code_key", schema)), budget, executor.DefaultRetryPolicy()))
	require.NoError(t, busy.Rollback(t.Context()))

	err = executor.ExecuteNative(t.Context(), pool, pPreflight,
		mustParse(t, fmt.Sprintf("ALTER TABLE %s.p DROP CONSTRAINT p_ref_key", schema)), budget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrReplicaIdentityDrop)
}
