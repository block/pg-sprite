package schemadiff_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A desired file for a partition names its live parent with PARTITION OF.
// Building it on the scratch schema would attach the scratch table to the
// live parent under ACCESS EXCLUSIVE, which queues behind any reader and
// blocks every read and write behind it, so the file is refused before
// the scratch build runs anything. A session holding ACCESS SHARE on the
// parent proves no lock is requested: a build that asked for one would
// wait on it until the deadline instead of returning the refusal.
func TestIntrospectDesiredRefusesPartitionOf(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprintf("public=%t", public), func(t *testing.T) {
			schema := "public"
			if !public {
				schema = testutil.NewSchema(t, pool)
			}
			_, err := pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TABLE %[1]s.metrics (id bigint NOT NULL, day date NOT NULL) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.metrics_p1 PARTITION OF %[1]s.metrics FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`, schema))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf("DROP TABLE %s.metrics", schema))
				require.NoError(t, err)
			})

			holder, err := pool.Begin(t.Context())
			require.NoError(t, err)
			defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
			_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.metrics IN ACCESS SHARE MODE", schema))
			require.NoError(t, err)

			ds, err := statement.ParseDesired("CREATE TABLE metrics_p1 PARTITION OF metrics FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err = schemadiff.IntrospectDesired(ctx, pool, schema, ds)
			require.ErrorIs(t, err, schemadiff.ErrDesiredPartitionOf)
		})
	}
}
