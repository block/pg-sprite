package suggest_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/suggest"
)

// The SQLSTATEs the scaffold-residue contract is defined by.
const (
	sqlstateCheckViolation  = "23514"
	sqlstateDuplicateObject = "42710"
)

// pgCode extracts the SQLSTATE from a PostgreSQL error; empty when the
// error is not a server error.
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// The scaffold-constraint-on-failure caveat describes real behavior: a
// failed VALIDATE leaves the NOT VALID scaffold on the live table, and
// replaying the recommended sequence then fails at the ADD CONSTRAINT step
// with duplicate_object. This pins the failure mode the caveat promises so
// the advice and the server can never drift apart.
func TestSetNotNullSequenceResidueMatchesCaveat(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.orders (id bigint PRIMARY KEY, paid_at timestamptz)", schema))
	require.NoError(t, err)
	// One NULL row — the ordinary case a VALIDATE discovers.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.orders VALUES (1, NULL)", schema))
	require.NoError(t, err)

	report, err := suggest.Advise(fmt.Sprintf(
		"ALTER TABLE %s.orders ALTER COLUMN paid_at SET NOT NULL", schema))
	require.NoError(t, err)
	require.Len(t, report.Suggestions, 1)
	s := report.Suggestions[0]
	assert.Contains(t, s.Caveats, suggest.CaveatScaffoldConstraintOnFailure,
		"the sequence whose residue this test proves must carry the caveat")
	require.Len(t, s.Recommended, 4)

	// Step 1 (ADD ... NOT VALID) succeeds; step 2 (VALIDATE) fails on the
	// NULL row.
	_, err = pool.Exec(t.Context(), s.Recommended[0])
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), s.Recommended[1])
	require.Equal(t, sqlstateCheckViolation, pgCode(err),
		"VALIDATE fails as check_violation on the NULL row")

	// The residue the caveat names: the scaffold constraint is still on
	// the live table.
	var scaffolds int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_constraint
		 WHERE connamespace = $1::regnamespace AND contype = 'c' AND NOT convalidated`,
		schema).Scan(&scaffolds))
	assert.Equal(t, 1, scaffolds, "a failed VALIDATE leaves the NOT VALID scaffold behind")

	// A naive replay of the sequence fails at step 1 with
	// duplicate_object — the retry behavior the caveat warns about.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"UPDATE %s.orders SET paid_at = now() WHERE paid_at IS NULL", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), s.Recommended[0])
	require.Equal(t, sqlstateDuplicateObject, pgCode(err),
		"replaying the sequence trips over the leftover scaffold")

	// The recovery the caveat prescribes: resume from the VALIDATE step;
	// the rest of the sequence completes.
	for _, step := range s.Recommended[1:] {
		_, err = pool.Exec(t.Context(), step)
		require.NoError(t, err)
	}
	var notNull bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT attnotnull FROM pg_attribute
		 WHERE attrelid = ($1 || '.orders')::regclass AND attname = 'paid_at'`,
		schema).Scan(&notNull))
	assert.True(t, notNull, "resuming from VALIDATE converges on the declared end state")
}
