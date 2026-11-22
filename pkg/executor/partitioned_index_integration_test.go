package executor_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

func TestBuildPartitionedIndex(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		forms := []struct{ name, stmt string }{
			{"plain", "CREATE INDEX idx_orders_total ON %s.orders (total)"},
			{"only", "CREATE INDEX idx_orders_total ON ONLY %s.orders (total)"},
			{"concurrently", "CREATE INDEX CONCURRENTLY idx_orders_total ON %s.orders (total)"},
		}
		for _, form := range forms {
			// Every form asks for a complete index on the whole tree: the
			// parent's index is created without building, each partition's
			// index is built under its derived name and attached, and the
			// report says what ran after the parent verified valid.
			t.Run("builds a complete index from the "+form.name+" form", func(t *testing.T) {
				f := newFixture(t)
				partitions := f.ordersByRegion(t)

				rep := f.build(t, "orders", fmt.Sprintf(form.stmt, f.schema))

				assert.Equal(t, f.schema, rep.Schema)
				assert.Equal(t, "idx_orders_total", rep.Index)
				assert.True(t, rep.ParentCreated)
				assert.Equal(t, 3, rep.Partitions)
				assert.NotZero(t, rep.IndexOID)
				assert.NotEmpty(t, rep.ServerVersion)
				assert.Positive(t, rep.Duration)
				require.Len(t, rep.Worked, 3)
				assert.ElementsMatch(t, partitions, workedTables(rep))
				for _, w := range rep.Worked {
					assert.Equal(t, executor.PartitionIndexBuilt, w.Action)
					assert.Equal(t, f.schema, w.Schema)
					assert.Equal(t, f.derived(t, "idx_orders_total", w.Table), w.Index)
					assert.NotZero(t, w.IndexOID)
				}
				f.requireCovered(t, "orders", "idx_orders_total", partitions...)
			})
		}

		// The partition indexes carry the whole definition the parent's
		// printed form carries. The expression calls a function in a schema
		// outside any search path the build uses, and every statement the
		// build derives still names it.
		t.Run("keeps the definition the request describes", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `CREATE SCHEMA %s_fn`, f.schema)
			f.exec(t, `CREATE FUNCTION %s_fn.shout(t text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT upper(t)'`, f.schema)
			f.exec(t, `CREATE TABLE %s.docs (id bigint NOT NULL, name text, body text, kind int) PARTITION BY RANGE (id)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.docs_a PARTITION OF %[1]s.docs FOR VALUES FROM (0) TO (100)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.docs_b PARTITION OF %[1]s.docs FOR VALUES FROM (100) TO (200)`, f.schema)
			f.exec(t, `INSERT INTO %s.docs SELECT g, 'n' || g, 'b', g FROM generate_series(1, 150) g`, f.schema)

			cases := []struct {
				index string
				stmt  string
				want  []string
			}{
				{
					"idx_docs_key",
					`CREATE UNIQUE INDEX idx_docs_key ON %[1]s.docs (id, lower(name) COLLATE "C" text_pattern_ops DESC NULLS LAST) INCLUDE (body) WITH (fillfactor=70) WHERE kind > 0`,
					[]string{"UNIQUE", `COLLATE "C"`, "text_pattern_ops", "DESC NULLS LAST", "INCLUDE (body)", "fillfactor='70'", "WHERE (kind > 0)"},
				},
				{
					"idx_docs_fn",
					`CREATE INDEX idx_docs_fn ON %[1]s.docs (%[1]s_fn.shout(name))`,
					[]string{fmt.Sprintf("%s_fn.shout", f.schema)},
				},
			}
			for _, c := range cases {
				rep := f.build(t, "docs", fmt.Sprintf(c.stmt, f.schema))
				assert.Equal(t, c.index, rep.Index)
				f.requireCovered(t, "docs", c.index, "docs_a", "docs_b")
				for _, w := range rep.Worked {
					def := f.indexesOn(t, w.Table)[w.Index].indexed
					for _, part := range c.want {
						assert.Contains(t, strings.ReplaceAll(def, "WITH (fillfactor='70')", "fillfactor='70'"), part,
							"partition %s index must keep %q", w.Table, part)
					}
				}
			}
		})

		// The build runs while a writer inserts into the table without a
		// pause, under a short lock_timeout of its own. The partitions hold
		// enough rows that the builds take a while, so the writer is
		// provably running during them: it must keep writing, with no gap
		// longer than a brief step and no lock timeout.
		t.Run("a concurrent writer is not blocked", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			f.exec(t, `INSERT INTO %s.orders SELECT g, r, g %% 1000, repeat('x', 40) FROM generate_series(1, 150000) g, (VALUES ('eu'), ('us'), ('ap')) v(r)`, f.schema)
			writerConn, err := pgx.Connect(t.Context(), f.cfg.URL)
			require.NoError(t, err)
			t.Cleanup(func() { _ = writerConn.Close(context.WithoutCancel(t.Context())) })
			_, err = writerConn.Exec(t.Context(), `SET lock_timeout = '1s'`)
			require.NoError(t, err)

			ctx, stop := context.WithCancel(t.Context())
			var writer sync.WaitGroup
			// Registered after the connection's closer, so it runs first on
			// every exit path and the writer is joined before its connection
			// closes.
			t.Cleanup(func() {
				stop()
				writer.Wait()
			})
			var writeTimes []time.Time
			var writeErr error
			started := make(chan struct{})
			writer.Go(func() {
				for ctx.Err() == nil {
					if _, err := writerConn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.orders VALUES (1000000, 'eu', 7, 'w')`, f.schema)); err != nil {
						if ctx.Err() == nil {
							writeErr = err
						}
						return
					}
					writeTimes = append(writeTimes, time.Now())
					if len(writeTimes) == 1 {
						close(started)
					}
				}
			})
			select {
			case <-started:
			case <-time.After(30 * time.Second):
				t.Fatal("the writer never wrote")
			}

			buildStart := time.Now()
			f.build(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema))
			buildEnd := time.Now()
			stop()
			writer.Wait()

			require.NoError(t, writeErr, "a write must never wait out its lock_timeout")
			var during []time.Time
			for _, w := range writeTimes {
				if !w.Before(buildStart) && !w.After(buildEnd) {
					during = append(during, w)
				}
			}
			require.GreaterOrEqual(t, len(during), 10, "the writer must have been writing while the build ran")
			var longest time.Duration
			for i := 1; i < len(during); i++ {
				longest = max(longest, during[i].Sub(during[i-1]))
			}
			assert.Less(t, longest, 500*time.Millisecond, "no write may wait behind the build")
			f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		})

		// The parent index name can be as long as the server allows: the
		// derived names are cut to fit, and the OID suffix keeps them apart.
		t.Run("a full-length index name gets distinct derived names", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			name := strings.Repeat("i", 63)

			rep := f.build(t, "orders", fmt.Sprintf("CREATE INDEX %s ON %s.orders (total)", name, f.schema))

			seen := map[string]bool{}
			for _, w := range rep.Worked {
				assert.LessOrEqual(t, len(w.Index), 63)
				assert.False(t, seen[w.Index], "derived names must be distinct")
				seen[w.Index] = true
				assert.True(t, strings.HasSuffix(w.Index, fmt.Sprintf("_p%d", f.tableOID(t, w.Table))))
			}
			f.requireCovered(t, "orders", name, partitions...)
		})

		// A partition that is itself partitioned gets its own metadata-only
		// index, filled by its partitions' indexes and attached upward, to
		// any depth. Leaves are built; the levels between are "partitioned".
		t.Run("builds through sub-partitioned partitions", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `CREATE TABLE %s.events (id bigint NOT NULL, region text NOT NULL, ts date NOT NULL) PARTITION BY RANGE (ts)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events FOR VALUES FROM ('2024-01-01') TO ('2025-01-01') PARTITION BY LIST (region)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_2024_eu PARTITION OF %[1]s.events_2024 FOR VALUES IN ('eu')`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_2024_us PARTITION OF %[1]s.events_2024 FOR VALUES IN ('us') PARTITION BY HASH (id)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_2024_us_h0 PARTITION OF %[1]s.events_2024_us FOR VALUES WITH (MODULUS 2, REMAINDER 0)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_2024_us_h1 PARTITION OF %[1]s.events_2024_us FOR VALUES WITH (MODULUS 2, REMAINDER 1)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_2025 PARTITION OF %[1]s.events FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')`, f.schema)
			f.exec(t, `INSERT INTO %s.events SELECT g, CASE g %% 2 WHEN 0 THEN 'eu' ELSE 'us' END, DATE '2024-03-01' + g FROM generate_series(1, 100) g`, f.schema)
			all := []string{"events_2024", "events_2024_eu", "events_2024_us", "events_2024_us_h0", "events_2024_us_h1", "events_2025"}

			rep := f.build(t, "events", fmt.Sprintf("CREATE INDEX idx_events_region ON ONLY %s.events (region, id)", f.schema))

			assert.Equal(t, 6, rep.Partitions)
			assert.ElementsMatch(t, all, workedTables(rep))
			actions := map[string]executor.PartitionIndexAction{}
			for _, w := range rep.Worked {
				actions[w.Table] = w.Action
			}
			assert.Equal(t, map[string]executor.PartitionIndexAction{
				"events_2024":       executor.PartitionIndexOnly,
				"events_2024_eu":    executor.PartitionIndexBuilt,
				"events_2024_us":    executor.PartitionIndexOnly,
				"events_2024_us_h0": executor.PartitionIndexBuilt,
				"events_2024_us_h1": executor.PartitionIndexBuilt,
				"events_2025":       executor.PartitionIndexBuilt,
			}, actions)
			f.requireCovered(t, "events", "idx_events_region", "events_2024", "events_2025")
			// Each sub-partitioned level is valid on its own only because
			// the level below it is covered.
			for _, p := range []string{"events_2024", "events_2024_us"} {
				for name, r := range f.indexesOn(t, p) {
					if r.parent != "" {
						assert.Equal(t, "I", r.kind, "%s.%s", p, name)
						assert.True(t, r.valid, "%s.%s must be valid", p, name)
					}
				}
			}
			f.requireCovered(t, "events_2024", f.derived(t, "idx_events_region", "events_2024"), "events_2024_eu", "events_2024_us")
			f.requireCovered(t, "events_2024_us", f.derived(t, "idx_events_region", "events_2024_us"), "events_2024_us_h0", "events_2024_us_h1")
		})

		// NULLS NOT DISTINCT is part of the printed definition from
		// PostgreSQL 15 on, so a unique index that sets it must keep it on
		// every partition. Earlier servers do not have the option, so the
		// case exists only where the server parses it.
		if serverVersionNum(t, newFixture(t)) >= 150000 {
			t.Run("keeps NULLS NOT DISTINCT", func(t *testing.T) {
				f := newFixture(t)
				partitions := f.ordersByRegion(t)

				rep := f.build(t, "orders", fmt.Sprintf("CREATE UNIQUE INDEX idx_orders_nnd ON %s.orders (region, id, total) NULLS NOT DISTINCT", f.schema))

				require.Len(t, rep.Worked, 3)
				for _, w := range rep.Worked {
					assert.Contains(t, f.indexesOn(t, w.Table)[w.Index].indexed, "NULLS NOT DISTINCT")
				}
				f.requireCovered(t, "orders", "idx_orders_nnd", partitions...)
			})
		}
	})
}
