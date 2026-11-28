package migrate_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// columnDropWriterStart bounds the wait for the forbidden writer's first
// attempts, so the apply provably runs while the writer is going.
const columnDropWriterStart = 10 * time.Second

// columnDropCase is a table whose file drops a column and changes a rule on
// the columns that remain. forbidden inserts a row that both the old rule
// and the new one reject, numbered n so each attempt is a distinct row.
type columnDropCase struct {
	fixture   string
	desired   string
	forbidden func(schema string, n int64) string
	rule      string
	wantDef   func(schema string) string
}

func columnDropTable(t *testing.T, fixture string) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(fixture, schema))
	require.NoError(t, err)
	return pool, schema
}

// writeForbidden inserts forbidden rows in a loop until ctx ends, and
// returns how many it attempted and how many the table accepted.
func writeForbidden(ctx context.Context, pool *pgxpool.Pool, schema string, forbidden func(string, int64) string, attempts, accepted *atomic.Int64) {
	for n := int64(1); ctx.Err() == nil; n++ {
		attempts.Add(1)
		if _, err := pool.Exec(ctx, forbidden(schema, n)); err == nil {
			accepted.Add(1)
		}
		time.Sleep(time.Millisecond)
	}
}

// A file that drops a column and changes a key, check, or foreign key on the
// columns that remain never lets in a row both rules reject: a writer
// inserting such rows for the whole apply gets none of them in. The new rule
// is in place before the column, and the old rule with it, goes.
func TestRunDesiredKeepsARuleWhileDroppingAColumn(t *testing.T) {
	for name, tc := range map[string]columnDropCase{
		"a check that read the dropped column": {
			fixture: `CREATE TABLE %[1]s.t (
				id int PRIMARY KEY,
				legacy int,
				CONSTRAINT t_rule CHECK (id > 0 AND legacy > 0)
			);
			INSERT INTO %[1]s.t SELECT g, 1 FROM generate_series(1, 1000) g`,
			desired: `CREATE TABLE t (id int PRIMARY KEY, CONSTRAINT t_rule CHECK (id > 0));`,
			forbidden: func(schema string, n int64) string {
				return fmt.Sprintf(`INSERT INTO %s.t (id) VALUES (%d)`, schema, -n)
			},
			rule: "t_rule", wantDef: func(string) string { return "CHECK ((id > 0))" },
		},
		"a unique key that loses the dropped column": {
			fixture: `CREATE TABLE %[1]s.t (
				id int PRIMARY KEY,
				email text,
				tenant int NOT NULL DEFAULT 1,
				CONSTRAINT t_email_key UNIQUE (email, tenant)
			);
			INSERT INTO %[1]s.t (id, email) SELECT g, 'u' || g FROM generate_series(1, 1000) g`,
			desired: `CREATE TABLE t (id int PRIMARY KEY, email text, CONSTRAINT t_email_key UNIQUE (email));`,
			forbidden: func(schema string, n int64) string {
				return fmt.Sprintf(`INSERT INTO %s.t (id, email) VALUES (%d, 'u1')`, schema, 100000+n)
			},
			rule: "t_email_key", wantDef: func(string) string { return "UNIQUE (email)" },
		},
		"a primary key that loses the dropped column": {
			fixture: `CREATE TABLE %[1]s.t (
				id int NOT NULL,
				tenant int NOT NULL DEFAULT 1,
				CONSTRAINT t_pkey PRIMARY KEY (id, tenant)
			);
			INSERT INTO %[1]s.t (id) SELECT g FROM generate_series(1, 1000) g`,
			desired: `CREATE TABLE t (id int NOT NULL, CONSTRAINT t_pkey PRIMARY KEY (id));`,
			forbidden: func(schema string, n int64) string {
				return fmt.Sprintf(`INSERT INTO %s.t (id) VALUES (%d)`, schema, 1+n%1000)
			},
			rule: "t_pkey", wantDef: func(string) string { return "PRIMARY KEY (id)" },
		},
		"a foreign key that loses the dropped column": {
			fixture: `CREATE TABLE %[1]s.parents (
				id int PRIMARY KEY,
				t int NOT NULL,
				CONSTRAINT parents_id_t_key UNIQUE (id, t)
			);
			INSERT INTO %[1]s.parents SELECT g, 1 FROM generate_series(1, 10) g;
			CREATE TABLE %[1]s.t (
				id int PRIMARY KEY,
				pid int,
				t int NOT NULL DEFAULT 1,
				CONSTRAINT t_parent_fk FOREIGN KEY (pid, t) REFERENCES %[1]s.parents (id, t)
			);
			INSERT INTO %[1]s.t (id, pid) SELECT g, 1 + g %% 10 FROM generate_series(1, 1000) g`,
			desired: `CREATE TABLE t (id int PRIMARY KEY, pid int, CONSTRAINT t_parent_fk FOREIGN KEY (pid) REFERENCES parents (id));`,
			forbidden: func(schema string, n int64) string {
				return fmt.Sprintf(`INSERT INTO %s.t (id, pid) VALUES (%d, 999)`, schema, 100000+n)
			},
			rule: "t_parent_fk", wantDef: func(schema string) string {
				return fmt.Sprintf("FOREIGN KEY (pid) REFERENCES %s.parents(id)", schema)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool, schema := columnDropTable(t, tc.fixture)
			reviewed := plannedDrop(t, pool, schema, tc.desired)

			ctx, stop := context.WithCancel(t.Context())
			var attempts, accepted atomic.Int64
			var wg sync.WaitGroup
			wg.Go(func() { writeForbidden(ctx, pool, schema, tc.forbidden, &attempts, &accepted) })
			require.Eventually(t, func() bool { return attempts.Load() >= 10 }, columnDropWriterStart, 10*time.Millisecond,
				"the writer is running before the apply starts")

			res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
				Schema: schema, Desired: parseDesired(t, tc.desired),
				ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
			}, runOptions())
			stop()
			wg.Wait()
			require.NoError(t, err)
			require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)

			assert.Zero(t, accepted.Load(), "no row both rules reject got in, of %d attempts", attempts.Load())
			def, validated := constraintDef(t, pool, schema, tc.rule)
			assert.Equal(t, tc.wantDef(schema), def)
			assert.True(t, validated)
			assert.Empty(t, plannedDrop(t, pool, schema, tc.desired).Statements, "the table converges")
		})
	}
}
