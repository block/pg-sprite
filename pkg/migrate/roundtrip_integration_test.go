package migrate_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Each case builds a live table, converges it to a desired file with
// RunDesired, and holds the result to the round-trip oracle: the converged
// table's catalog record must match a table built directly from the desired
// file, and a second run must plan nothing. The catalog comparison is what
// a re-plan alone cannot give: it reads the server's catalogs directly, so
// a property the diff model ignores on both sides still fails the case
// instead of re-planning to silence.
func TestRunDesiredRoundTrip(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("adds a nullable column", func(t *testing.T) {
		live := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    name text NOT NULL
);`
		desired := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    name text NOT NULL,
    note text
);`
		res := assertRoundTrip(t, pool, live, desired)
		assert.Equal(t, []string{"ALTER TABLE t ADD COLUMN note text"}, plannedSQL(t, res))
	})

	t.Run("adds a column with a constant default", func(t *testing.T) {
		// A constant default is stored in the catalog, not written into
		// every row, so the add stays a catalog-only change.
		live := `CREATE TABLE t (
    id bigint PRIMARY KEY
);`
		desired := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    status text NOT NULL DEFAULT 'active'
);`
		res := assertRoundTrip(t, pool, live, desired)
		assert.Equal(t, []string{"ALTER TABLE t ADD COLUMN status text DEFAULT ('active'::text) NOT NULL"}, plannedSQL(t, res))
	})

	t.Run("adds an index", func(t *testing.T) {
		// The index is built concurrently; the oracle also proves the build
		// finished valid, since the reference index is valid.
		live := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    email text
);`
		desired := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    email text
);
CREATE INDEX t_email_idx ON t (email);`
		res := assertRoundTrip(t, pool, live, desired)
		assert.Equal(t, []string{"CREATE INDEX t_email_idx ON t USING btree (email)"}, plannedSQL(t, res))
	})

	t.Run("sets a column NOT NULL", func(t *testing.T) {
		// The planner runs SET NOT NULL as an online sequence: a NOT VALID
		// check, its validation, then SET NOT NULL, which the validated
		// check lets skip the full-table scan. The oracle proves the helper
		// check is gone afterwards: the reference table never had one.
		live := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    email text
);`
		desired := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    email text NOT NULL
);`
		res := assertRoundTrip(t, pool, live, desired)
		assert.Equal(t, []string{"ALTER TABLE t ALTER COLUMN email SET NOT NULL"}, plannedSQL(t, res))
	})

	t.Run("adds a check constraint", func(t *testing.T) {
		// The planner adds the check NOT VALID and validates it separately,
		// so the scan runs without blocking writes. The oracle proves the
		// constraint ends validated, matching the reference.
		live := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    amount numeric(12,2)
);`
		desired := `CREATE TABLE t (
    id bigint PRIMARY KEY,
    amount numeric(12,2),
    CONSTRAINT t_amount_check CHECK (amount >= 0)
);`
		res := assertRoundTrip(t, pool, live, desired)
		assert.Equal(t, []string{"ALTER TABLE t ADD CONSTRAINT t_amount_check CHECK (amount >= 0::numeric)"}, plannedSQL(t, res))
	})

	t.Run("adds a column with quoted, mixed-case names", func(t *testing.T) {
		// Quoting has to survive every step: the planned SQL, the scratch
		// build, and execution. An unquoted name would fold to lower case
		// and land as a different column and index.
		live := `CREATE TABLE "Orders" (
    "Id" bigint PRIMARY KEY
);`
		desired := `CREATE TABLE "Orders" (
    "Id" bigint PRIMARY KEY,
    "Customer Ref" text
);
CREATE INDEX "Orders_Customer_Ref_idx" ON "Orders" ("Customer Ref");`
		res := assertRoundTripTable(t, pool, "Orders", live, desired)
		assert.Equal(t, []string{
			`ALTER TABLE "Orders" ADD COLUMN "Customer Ref" text`,
			`CREATE INDEX "Orders_Customer_Ref_idx" ON "Orders" USING btree ("Customer Ref")`,
		}, plannedSQL(t, res))
	})
}

// assertRoundTrip runs the round-trip oracle for a table named t.
func assertRoundTrip(t *testing.T, pool *pgxpool.Pool, liveSQL, desiredSQL string) migrate.DesiredResult {
	t.Helper()
	return assertRoundTripTable(t, pool, "t", liveSQL, desiredSQL)
}

// assertRoundTripTable builds liveSQL in one schema and desiredSQL in
// another, converges the live table with RunDesired, and asserts that the
// run executed, that the converged table's catalog record equals the
// reference table's, and that a second run plans nothing. It returns the
// first run's result so the caller can assert the plan it expects.
func assertRoundTripTable(t *testing.T, pool *pgxpool.Pool, table, liveSQL, desiredSQL string) migrate.DesiredResult {
	t.Helper()
	liveSchema := testutil.NewSchema(t, pool)
	referenceSchema := testutil.NewSchema(t, pool)
	execInSchema(t, pool, liveSchema, liveSQL)
	execInSchema(t, pool, referenceSchema, desiredSQL)

	req := migrate.DesiredRequest{Schema: liveSchema, Desired: parseDesired(t, desiredSQL)}
	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s: %s", res.Reason, res.Detail)
	require.NotEmpty(t, res.Plan.Statements, "the live table must start out different from the desired file")

	reference, err := testutil.ReadTableCatalog(t.Context(), pool, referenceSchema, table)
	require.NoError(t, err)
	converged, err := testutil.ReadTableCatalog(t.Context(), pool, liveSchema, table)
	require.NoError(t, err)
	assert.Equal(t, reference, converged,
		"the converged table differs from one built from the desired file; plan:\n%v", plannedSQL(t, res))

	again, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Empty(t, plannedSQL(t, again), "the converged table must plan no statements")
	return res
}

// execInSchema runs sql with search_path set to schema, so the unqualified
// fixture SQL lands there.
func execInSchema(t *testing.T, pool *pgxpool.Pool, schema, sql string) {
	t.Helper()
	err := pgx.BeginTxFunc(t.Context(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL search_path = "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), sql)
		return err
	})
	require.NoError(t, err, sql)
}

// plannedSQL lists the plan's statements with the target schema stripped,
// so the expected SQL reads the same as the fixture.
func plannedSQL(t *testing.T, res migrate.DesiredResult) []string {
	t.Helper()
	var out []string
	for _, st := range res.Plan.Statements {
		sql, err := statement.Qualify(st.SQL, "")
		require.NoError(t, err, st.SQL)
		out = append(out, sql)
	}
	return out
}
