package migrate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// An adoption is diff-planned SQL, which is never forced, so a forced
// attempt that carries one is a breach of the plan's contract. It refuses
// before the table is even read: the pool is never touched.
func TestExecuteRefusesAForcedAttemptThatAdoptsAnIndex(t *testing.T) {
	st, err := statement.ParseOne(`ALTER TABLE s.t ADD CONSTRAINT t_email_key UNIQUE USING INDEX t_email_key`)
	require.NoError(t, err)
	adopts := &schemadiff.IndexAdoption{Index: "t_email_key", Def: "CREATE UNIQUE INDEX t_email_key ON t USING btree (email)"}

	_, err = execute(t.Context(), nil, st, []string{st.SQL()}, planner.Plan{}, false, true, Options{}, adopts, nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	require.ErrorContains(t, err, "a forced attempt cannot carry the adoption of index t_email_key")
}
