package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
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

// An admitted plan turns the unique constraint t_email_key into a unique
// index of the same name: it builds t_email_key_new, then drops the
// constraint. By the time the drop runs, the build was dropped. The plan's
// drop step refuses with replacement-index-drift and leaves the constraint
// enforcing its rule; once the build is back, the same step drops it.
func TestRunRefusesAConstraintDropWhoseReplacementIsGone(t *testing.T) {
	f := newCopySwapFixture(t)
	f.exec(t, `CREATE TABLE {schema}.t (id int PRIMARY KEY, email text CONSTRAINT t_email_key UNIQUE)`)
	desired, err := statement.ParseDesired(`CREATE TABLE t (id int PRIMARY KEY, email text);
		CREATE UNIQUE INDEX t_email_key ON t (lower(email));`)
	require.NoError(t, err)
	report, err := diffplan.Plan(t.Context(), f.pool, diffplan.Request{Schema: f.schema, Desired: desired})
	require.NoError(t, err)
	require.Len(t, report.Statements, 3)
	build, drop := report.Statements[0], report.Statements[1]
	require.Equal(t, schemadiff.ChangeDropConstraint, drop.Kind)
	require.NotNil(t, drop.Replacement, "the plan's drop carries the replacement it checks")
	dropped, err := droppedWithReplacement(drop)
	require.NoError(t, err)
	require.Equal(t, drop.Replacement, dropped.Replacement)
	st, err := statement.ParseOne(drop.SQL)
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.AllowDestructive = f.schema + ".t"

	v, err := run(t.Context(), f.pool, st, opts, 2, nil, nil, dropped, nil)
	require.ErrorIs(t, err, executor.ErrReplacementIndexDrift)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	var kept int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint
		WHERE conrelid = format('%I.t', $1::text)::regclass AND conname = 't_email_key'`, f.schema).Scan(&kept))
	assert.Equal(t, 1, kept, "the constraint still enforces its rule")

	f.exec(t, build.SQL)
	v, err = run(t.Context(), f.pool, st, opts, 2, nil, nil, dropped, nil)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
}

// A plan whose drop names a replacement it does not carry, or carries one on
// a drop that is not a unique constraint's, runs nothing.
func TestDroppedWithReplacementRefusesAMismatchedPlan(t *testing.T) {
	replacement := &schemadiff.IndexReplacement{Index: "t_email_key_new", Def: "CREATE UNIQUE INDEX t_email_key_new ON t USING btree (email)"}
	for name, ps := range map[string]plan.Statement{
		"named, not carried": {SQL: "ALTER TABLE s.t DROP CONSTRAINT t_email_key", Kind: schemadiff.ChangeDropConstraint, ReplacedBy: "t_email_key_new",
			Drops: &schemadiff.DroppedObject{Kind: schemadiff.DroppedConstraint, Name: "t_email_key", Definition: "UNIQUE (email)"}},
		"on a column drop": {SQL: "ALTER TABLE s.t DROP COLUMN email", Kind: schemadiff.ChangeDropColumn, ReplacedBy: "t_email_key_new", Replacement: replacement,
			Drops: &schemadiff.DroppedObject{Kind: schemadiff.DroppedColumn, Name: "email", Definition: "email text"}},
	} {
		_, err := droppedWithReplacement(ps)
		require.ErrorIs(t, err, executor.ErrInvariantViolation, name)
	}
}
