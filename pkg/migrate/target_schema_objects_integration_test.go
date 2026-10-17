package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file can add an expression index over a function the target
// schema defines, naming it bare. The concurrent build resolves the name in
// the target schema, the way the plan printed it, so the index depends on
// the target schema's function even when public defines one of the same
// name that the session's own search_path would find first. A re-run
// plans nothing.
func TestRunDesiredBuildsAnIndexOverTheTargetSchemasFunction(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE FUNCTION public.norm(s text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT upper(s)';"+
			" CREATE FUNCTION %[1]s.norm(s text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT lower(s)';"+
			" CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, e text)", schema))
	require.NoError(t, err)

	req := migrate.DesiredRequest{
		Schema:  schema,
		Desired: parseDesired(t, "CREATE TABLE t (id bigint PRIMARY KEY, e text);\nCREATE INDEX t_norm_idx ON t (norm(e));"),
	}
	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	require.Len(t, res.Verdicts, 1)

	var functionSchema string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT n.nspname
		FROM pg_depend d
		JOIN pg_proc p ON p.oid = d.refobjid
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_proc'::regclass
		  AND d.objid = $1::regclass`, schema+".t_norm_idx").Scan(&functionSchema))
	assert.Equal(t, schema, functionSchema, "the index is built over the target schema's norm, not public's")

	res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	assert.Empty(t, res.Plan.Statements, "the built index matches the file")
}
