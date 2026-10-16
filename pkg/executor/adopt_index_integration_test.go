package executor_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A planned ADD CONSTRAINT ... USING INDEX names only the index, so the
// step checks the index against the definition the plan matched before it
// attaches it. An index replaced under the same name after planning, or
// dropped, refuses with the permanent adopted-index-drift code and no
// constraint is added; the index the plan matched is taken over.
func TestRunSequenceAdoptingRefusesAnIndexReplacedAfterPlanning(t *testing.T) {
	pool, schema := newPool(t)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id bigint, email text NOT NULL, code text NOT NULL)",
		"CREATE UNIQUE INDEX t_email_key ON %[1]s.t (email)",
		"CREATE UNIQUE INDEX t_code_key ON %[1]s.t (code)",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	pt := mustPreflight(t, pool, schema, "t")
	adopt := func(index, def string) error {
		t.Helper()
		_, err := executor.RunSequenceAdopting(t.Context(), pool, pt,
			[]string{fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT %s UNIQUE USING INDEX %s", schema, index, index)},
			runBudget, executor.DefaultRetryPolicy(),
			schemadiff.IndexAdoption{Index: index, Def: def})
		return err
	}
	constraints := func(name string) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT count(*) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2", schema+".t", name).Scan(&n))
		return n
	}

	// The index is replaced after the plan matched it: same name, another
	// column.
	for _, ddl := range []string{
		"DROP INDEX %[1]s.t_email_key",
		"CREATE UNIQUE INDEX t_email_key ON %[1]s.t (code, email)",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	err := adopt("t_email_key", "CREATE UNIQUE INDEX t_email_key ON t USING btree (email)")
	require.ErrorIs(t, err, executor.ErrAdoptedIndexDrift)
	assert.Equal(t, executor.CodeAdoptedIndexDrift, executor.OutcomeCode(err))
	assert.Contains(t, err.Error(), "USING btree (code, email)")
	assert.Equal(t, 0, constraints("t_email_key"), "the replacement index was not attached")

	_, err = pool.Exec(t.Context(), fmt.Sprintf("DROP INDEX %s.t_email_key", schema))
	require.NoError(t, err)
	err = adopt("t_email_key", "CREATE UNIQUE INDEX t_email_key ON t USING btree (email)")
	require.ErrorIs(t, err, executor.ErrAdoptedIndexDrift)
	assert.Contains(t, err.Error(), "no index named t_email_key")

	require.NoError(t, adopt("t_code_key", "CREATE UNIQUE INDEX t_code_key ON t USING btree (code)"))
	assert.Equal(t, 1, constraints("t_code_key"))
}

// A planned adoption no step of the sequence performs is a breach of the
// plan's contract: it refuses before anything runs.
func TestRunSequenceAdoptingRefusesASequenceThatAdoptsNothing(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id bigint, note text)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	_, err = executor.RunSequenceAdopting(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema)},
		runBudget, executor.DefaultRetryPolicy(),
		schemadiff.IndexAdoption{Index: "t_note_key", Def: "CREATE UNIQUE INDEX t_note_key ON t USING btree (note)"})
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.Equal(t, "text", columnType(t, pool, schema, "t", "note"), "nothing ran")
}

// A unique index left invalid by a failed concurrent build keeps its name
// and its definition, so the plan's definition still matches it. The step
// reads the index's validity under the lock as well, and refuses with the
// permanent adopted-index-drift code instead of attaching it.
func TestRunSequenceAdoptingRefusesAnInvalidIndex(t *testing.T) {
	pool, schema := newPool(t)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id bigint, email text NOT NULL)",
		"INSERT INTO %[1]s.t VALUES (1, 'a'), (2, 'a')",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY t_email_key ON %s.t (email)", schema))
	require.Error(t, err, "the duplicate values fail the build")
	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass", schema+".t_email_key").Scan(&valid))
	require.False(t, valid, "the failed build leaves the index behind, invalid")

	pt := mustPreflight(t, pool, schema, "t")
	_, err = executor.RunSequenceAdopting(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT t_email_key UNIQUE USING INDEX t_email_key", schema)},
		runBudget, executor.DefaultRetryPolicy(),
		schemadiff.IndexAdoption{Index: "t_email_key", Def: "CREATE UNIQUE INDEX t_email_key ON t USING btree (email)"})
	require.ErrorIs(t, err, executor.ErrAdoptedIndexDrift)
	assert.Equal(t, executor.CodeAdoptedIndexDrift, executor.OutcomeCode(err))
	assert.Contains(t, err.Error(), "is invalid")
}
