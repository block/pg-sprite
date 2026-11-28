package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// An admitted plan replaces t_email_key with a narrower index: it builds
// t_email_key_new, then drops t_email_key. By the time the drop runs, the
// replacement was rebuilt without the plan's predicate. The plan's drop
// step refuses with replacement-index-drift and leaves the old index
// enforcing its rule; once the replacement is the one the plan built, the
// same step drops the old index.
func TestDropPlannedIndexRefusesAReplacementThatChanged(t *testing.T) {
	f := newCopySwapFixture(t)
	f.exec(t, `CREATE TABLE {schema}.t (id int PRIMARY KEY, email text, deleted_at timestamptz);
		CREATE UNIQUE INDEX t_email_key ON {schema}.t (email)`)
	desired, err := statement.ParseDesired(`CREATE TABLE t (id int PRIMARY KEY, email text, deleted_at timestamptz);
		CREATE UNIQUE INDEX t_email_key ON t (email) WHERE deleted_at IS NULL;`)
	require.NoError(t, err)
	report, err := diffplan.Plan(t.Context(), f.pool, diffplan.Request{Schema: f.schema, Desired: desired})
	require.NoError(t, err)
	require.Len(t, report.Statements, 3)
	build, drop := report.Statements[0], report.Statements[1]
	require.NotNil(t, drop.Replacement, "the plan's drop carries the replacement it checks")
	st, err := statement.ParseOne(drop.SQL)
	require.NoError(t, err)
	opts := f.options()

	f.exec(t, `CREATE UNIQUE INDEX t_email_key_new ON {schema}.t (email)`)
	v, err := dropPlannedIndex(t.Context(), f.pool, report, drop, st, opts, 2)
	require.ErrorIs(t, err, executor.ErrReplacementIndexDrift)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeReplacementIndexDrift), v.Code)
	assert.Contains(t, v.Detail, "nothing was dropped")
	var valid bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 't_email_key'`, f.schema).Scan(&valid))
	assert.True(t, valid, "the old index still enforces its rule")

	f.exec(t, `DROP INDEX {schema}.t_email_key_new`)
	f.exec(t, build.SQL)
	v, err = dropPlannedIndex(t.Context(), f.pool, report, drop, st, opts, 2)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	var left int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = 't_email_key'`, f.schema).Scan(&left))
	assert.Zero(t, left, "the old index is dropped")
}
