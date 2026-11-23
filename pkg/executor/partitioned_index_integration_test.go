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

		// A rerun of a finished build has nothing to do: it creates nothing,
		// lists no partition as worked, and still confirms the tree and
		// verifies the parent.
		t.Run("a rerun of a finished build changes nothing", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON ONLY %s.orders (total)", f.schema)
			first := f.build(t, "orders", stmt)

			second := f.build(t, "orders", stmt)

			assert.False(t, second.ParentCreated)
			assert.Empty(t, second.Worked)
			assert.Equal(t, 3, second.Partitions)
			assert.Equal(t, first.IndexOID, second.IndexOID)
			assert.Equal(t, 2, second.Passes, "one pass, then the confirmation that rereads the tree")
			f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		})

		// A crash leaves a valid prefix: the parent's invalid index, one
		// partition attached, one with an index built under the derived
		// name but not attached. A rerun takes each as it is.
		t.Run("resumes what an earlier run left", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			f.exec(t, `CREATE INDEX idx_orders_total ON ONLY %s.orders USING btree (total)`, f.schema)
			attached := f.derived(t, "idx_orders_total", "orders_eu")
			f.exec(t, `CREATE INDEX %s ON %s.orders_eu (total)`, attached, f.schema)
			f.exec(t, `ALTER INDEX %s.idx_orders_total ATTACH PARTITION %s.%s`, f.schema, f.schema, attached)
			unattached := f.derived(t, "idx_orders_total", "orders_us")
			f.exec(t, `CREATE INDEX %s ON %s.orders_us (total)`, unattached, f.schema)
			require.False(t, f.indexesOn(t, "orders")["idx_orders_total"].valid, "the parent index is invalid until every partition is attached")

			rep := f.build(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema))

			assert.False(t, rep.ParentCreated)
			assert.Equal(t, 3, rep.Partitions)
			require.Len(t, rep.Worked, 2)
			byTable := map[string]executor.PartitionIndexResult{}
			for _, w := range rep.Worked {
				byTable[w.Table] = w
			}
			assert.Equal(t, executor.PartitionIndexReused, byTable["orders_us"].Action)
			assert.Equal(t, unattached, byTable["orders_us"].Index)
			assert.Equal(t, executor.PartitionIndexBuilt, byTable["orders_ap"].Action)
			f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		})

		// The partition's own index with the requested definition is adopted
		// under its own name, and the report says so. From then on it is a
		// partition of the parent's index. One that differs, or that backs a
		// constraint, is left alone and a new index is built.
		t.Run("adopts a matching index and leaves the others alone", func(t *testing.T) {
			f := newFixture(t)
			f.ordersByRegion(t)
			f.exec(t, `CREATE INDEX orders_eu_total ON %s.orders_eu (total)`, f.schema)
			f.exec(t, `CREATE INDEX orders_us_total_desc ON %s.orders_us (total DESC)`, f.schema)
			f.exec(t, `ALTER TABLE %s.orders_ap ADD CONSTRAINT orders_ap_total_key UNIQUE (total, region, id)`, f.schema)
			f.exec(t, `CREATE UNIQUE INDEX orders_ap_total_idx ON %s.orders_ap (total, region, id)`, f.schema)

			rep := f.build(t, "orders", fmt.Sprintf("CREATE UNIQUE INDEX idx_orders_uq ON ONLY %s.orders (total, region, id)", f.schema))

			byTable := map[string]executor.PartitionIndexResult{}
			for _, w := range rep.Worked {
				byTable[w.Table] = w
			}
			assert.Equal(t, executor.PartitionIndexBuilt, byTable["orders_eu"].Action, "a non-unique index is not the unique one requested")
			assert.Equal(t, executor.PartitionIndexBuilt, byTable["orders_us"].Action)
			assert.Equal(t, executor.PartitionIndexAdopted, byTable["orders_ap"].Action, "the plain unique index with the same key is adopted")
			assert.Equal(t, "orders_ap_total_idx", byTable["orders_ap"].Index)
			f.requireCovered(t, "orders", "idx_orders_uq", "orders_eu", "orders_us", "orders_ap")
			assert.Empty(t, f.indexesOn(t, "orders_ap")["orders_ap_total_key"].parent, "the constraint's index is not attached")
			assert.Empty(t, f.indexesOn(t, "orders_eu")["orders_eu_total"].parent)
			assert.Empty(t, f.indexesOn(t, "orders_us")["orders_us_total_desc"].parent)
		})

		// Only a valid, ready index that is attached to no other partitioned
		// index and matches the requested definition exactly is adopted. An
		// index attached under another parent, an invalid one, and ones that
		// differ only by a predicate or an INCLUDE column are left as they
		// were, and the build makes its own index beside them.
		t.Run("leaves alone an index it cannot adopt", func(t *testing.T) {
			f := newFixture(t)
			f.ordersByRegion(t)
			f.exec(t, `CREATE INDEX other_total ON ONLY %s.orders (total)`, f.schema)
			f.exec(t, `CREATE INDEX orders_eu_other ON %s.orders_eu (total)`, f.schema)
			f.exec(t, `ALTER INDEX %[1]s.other_total ATTACH PARTITION %[1]s.orders_eu_other`, f.schema)
			f.exec(t, `CREATE INDEX orders_us_broken ON %s.orders_us (total)`, f.schema)
			f.exec(t, `UPDATE pg_catalog.pg_index SET indisvalid = false WHERE indexrelid = '%s.orders_us_broken'::regclass`, f.schema)
			f.exec(t, `CREATE INDEX orders_ap_partial ON %s.orders_ap (total) WHERE total > 0`, f.schema)
			f.exec(t, `CREATE INDEX orders_ap_include ON %s.orders_ap (total) INCLUDE (note)`, f.schema)

			rep := f.build(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema))

			byTable := map[string]executor.PartitionIndexResult{}
			for _, w := range rep.Worked {
				byTable[w.Table] = w
			}
			for _, p := range []string{"orders_eu", "orders_us", "orders_ap"} {
				assert.Equal(t, executor.PartitionIndexBuilt, byTable[p].Action, p)
				assert.Equal(t, f.derived(t, "idx_orders_total", p), byTable[p].Index, p)
			}
			f.requireCovered(t, "orders", "idx_orders_total", "orders_eu", "orders_us", "orders_ap")
			assert.Equal(t, "other_total", f.indexesOn(t, "orders_eu")["orders_eu_other"].parent, "the index stays under its own parent")
			broken := f.indexesOn(t, "orders_us")["orders_us_broken"]
			assert.Empty(t, broken.parent)
			assert.False(t, broken.valid, "the invalid index is left as it was")
			assert.Empty(t, f.indexesOn(t, "orders_ap")["orders_ap_partial"].parent)
			assert.Empty(t, f.indexesOn(t, "orders_ap")["orders_ap_include"].parent)
		})

		// A partition created after the parent's index exists comes with an
		// index already attached, cloned by the server, so a rerun finds
		// nothing to do for it.
		t.Run("a partition created later is covered by the server", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)
			f.build(t, "orders", stmt)
			f.exec(t, `CREATE TABLE %[1]s.orders_sa PARTITION OF %[1]s.orders FOR VALUES IN ('sa')`, f.schema)

			rep := f.build(t, "orders", stmt)

			assert.Equal(t, 4, rep.Partitions)
			assert.Empty(t, rep.Worked)
			f.requireCovered(t, "orders", "idx_orders_total", append(partitions, "orders_sa")...)
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
