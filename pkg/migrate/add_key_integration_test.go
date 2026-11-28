package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A primary key or unique constraint a desired file adds to a live table
// with rows runs as a concurrent build of its index, then ADD CONSTRAINT
// ... USING INDEX. The plan records the index the attach takes over, with
// the definition its build leaves, so the attach checks it under the
// table's lock and never takes over an index put under that name between
// the two steps. A unique constraint on a column the same plan adds runs
// the same way, after the column is added. The run converges, and a second run plans nothing.
func TestRunDesiredAddsAKeyToALiveTable(t *testing.T) {
	for _, tt := range []struct {
		name, column, key, index, def, build, attach string
	}{
		{
			name:   "unique constraint",
			key:    "CONSTRAINT events_name_key UNIQUE (name) INCLUDE (email)",
			index:  "events_name_key",
			def:    "CREATE UNIQUE INDEX events_name_key ON events USING btree (name) INCLUDE (email)",
			build:  `CREATE UNIQUE INDEX CONCURRENTLY "events_name_key" ON "%s"."events" ("name") INCLUDE ("email")`,
			attach: `ALTER TABLE "%s"."events" ADD CONSTRAINT "events_name_key" UNIQUE USING INDEX "events_name_key"`,
		},
		{
			name:   "unique constraint on a column the plan adds",
			column: "code text,",
			key:    "CONSTRAINT events_code_key UNIQUE (code)",
			index:  "events_code_key",
			def:    "CREATE UNIQUE INDEX events_code_key ON events USING btree (code)",
			build:  `CREATE UNIQUE INDEX CONCURRENTLY "events_code_key" ON "%s"."events" ("code")`,
			attach: `ALTER TABLE "%s"."events" ADD CONSTRAINT "events_code_key" UNIQUE USING INDEX "events_code_key"`,
		},
		{
			name:   "primary key",
			key:    "CONSTRAINT events_pkey PRIMARY KEY (id)",
			index:  "events_pkey",
			def:    "CREATE UNIQUE INDEX events_pkey ON events USING btree (id)",
			build:  `CREATE UNIQUE INDEX CONCURRENTLY "events_pkey" ON "%s"."events" ("id")`,
			attach: `ALTER TABLE "%s"."events" ADD CONSTRAINT "events_pkey" PRIMARY KEY USING INDEX "events_pkey"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			schema := testutil.NewSchema(t, pool)
			_, err = pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TABLE %[1]s.events (id bigint NOT NULL, email text, name text);
				INSERT INTO %[1]s.events SELECT g, 'u' || g || '@example.com', 'n' || g FROM generate_series(1, 1000) g`, schema))
			require.NoError(t, err)
			req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, fmt.Sprintf(`
CREATE TABLE events (
    id bigint NOT NULL,
    email text,
    name text,
    %s
    %s
);`, tt.column, tt.key))}

			res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
			assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceVerified}, res.Convergence)
			require.NotEmpty(t, res.Plan.Statements)
			st := res.Plan.Statements[len(res.Plan.Statements)-1]
			assert.Equal(t, schemadiff.ChangeAddConstraint, st.Kind)
			assert.Equal(t, []string{
				fmt.Sprintf(tt.build, schema),
				fmt.Sprintf(tt.attach, schema),
			}, st.ExecSQL, "the key runs as a concurrent build, then an attach")
			assert.Equal(t, &schemadiff.IndexAdoption{Index: tt.index, Def: tt.def}, st.Adopts,
				"the attach checks the index its build left")

			var owned bool
			require.NoError(t, pool.QueryRow(t.Context(), `
				SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint con
				               JOIN pg_catalog.pg_class i ON i.oid = con.conindid
				               WHERE con.conrelid = $1::regclass AND con.conname = $2 AND i.relname = $2)`,
				schema+".events", tt.index).Scan(&owned))
			assert.True(t, owned, "the constraint owns the index the run built")

			res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
			assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
		})
	}
}
