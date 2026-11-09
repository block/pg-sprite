package migrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// irreversibleScenario is one registry entry's statement against the orders
// table, with the options that set every other gate open so the entry's own
// acknowledgement decides the outcome.
type irreversibleScenario struct {
	// sql is the statement, with {schema} standing for the table's schema.
	sql string
	// options returns the run's policy with the entry's acknowledgement set
	// to ack (empty for none); table names the orders table, for the
	// acknowledgements the statement also needs but the entry is not about.
	options func(cfg dbconn.Config, table, ack string) Options
	// cause is the verdict.Cause the refusal carries when ack is missing:
	// CauseNone for a gate that refuses on the statement alone, and the
	// gate's own cause where one stands behind the registry reason. Pinning
	// it keeps the subtest on the entry's gate, not on an earlier one that
	// refuses under the same reason.
	cause verdict.Cause
}

// irreversibleScenarios maps every registry key to its scenario. Each SQL
// statement, run with its acknowledgement naming the table, would discard
// what the entry's loss names.
func irreversibleScenarios() map[string]irreversibleScenario {
	allowDestructive := func(_ dbconn.Config, _, ack string) Options {
		opts := DefaultOptions()
		opts.AllowDestructive = ack
		return opts
	}
	// The route is opened: closed, every copy-and-swap statement is refused
	// under ReasonCopyAndSwapRefused with CauseCopyAndSwapNotEnabled before
	// the entry's own gate runs, and its scenario would pass without
	// exercising the acknowledgement it pins.
	copySwap := func(cfg dbconn.Config) Options {
		opts := DefaultOptions()
		opts.CopySwap = CopySwapOptions{Enabled: true, Connection: cfg, FreeDiskBytes: testutil.UnlimitedDisk}
		return opts
	}
	return map[string]irreversibleScenario{
		"shape: " + statement.Op{Kind: statement.OpDropColumn}.Describe(): {
			sql:     `ALTER TABLE {schema}.orders DROP COLUMN note`,
			options: allowDestructive,
		},
		"shape: " + statement.Op{Kind: statement.OpDropConstraint}.Describe(): {
			sql:     `ALTER TABLE {schema}.orders DROP CONSTRAINT orders_qty_positive`,
			options: allowDestructive,
		},
		"shape: " + statement.Op{Kind: statement.OpDropNotNull}.Describe(): {
			sql:     `ALTER TABLE {schema}.orders ALTER COLUMN note DROP NOT NULL`,
			options: allowDestructive,
		},
		// The blocking drop is accepted, so only the loss is left to
		// acknowledge.
		"shape: " + statement.Op{Kind: statement.OpDropIndex}.Describe(): {
			sql: `DROP INDEX {schema}.orders_qty_idx`,
			options: func(_ dbconn.Config, table, ack string) Options {
				opts := DefaultOptions()
				opts.AcceptBlocking = table
				opts.AllowDestructive = ack
				return opts
			},
		},
		// The loss is acknowledged, so only the lock is left to accept.
		"blocking DROP INDEX": {
			sql: `DROP INDEX {schema}.orders_qty_idx`,
			options: func(_ dbconn.Config, table, ack string) Options {
				opts := DefaultOptions()
				opts.AcceptBlocking = ack
				opts.AllowDestructive = table
				return opts
			},
		},
		"blocking REINDEX": {
			sql: `REINDEX INDEX {schema}.orders_qty_idx`,
			options: func(_ dbconn.Config, _, ack string) Options {
				opts := DefaultOptions()
				opts.AcceptBlocking = ack
				return opts
			},
		},
		// A column added with an inline UNIQUE builds its index under the
		// table's lock; the planner has no safer sequence for it.
		"submitted form that blocks the table": {
			sql: `ALTER TABLE {schema}.orders ADD COLUMN nickname text UNIQUE`,
			options: func(_ dbconn.Config, _, ack string) Options {
				opts := DefaultOptions()
				opts.Force = ack
				return opts
			},
		},
		// Writes are blocked, so the server needs no logical decoding and
		// only the restart is left to acknowledge.
		"copy-and-swap restart": {
			sql: `ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`,
			options: func(cfg dbconn.Config, table, ack string) Options {
				opts := copySwap(cfg)
				opts.CopySwap.BlockWrites = table
				opts.CopySwap.Restart = ack
				return opts
			},
		},
		// The test server runs with wal_level=replica, so without blocked
		// writes the route has no way to carry the table's changes, and the
		// refusal names the missing logical decoding.
		"copy-and-swap with the table's writes blocked": {
			sql: `ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`,
			options: func(cfg dbconn.Config, _, ack string) Options {
				opts := copySwap(cfg)
				opts.CopySwap.BlockWrites = ack
				return opts
			},
			cause: verdict.Cause(preflight.CopySwapCauseLogicalDecodingUnavailable),
		},
	}
}

// Every irreversible operation is refused, with the registry's reason,
// when its acknowledgement is missing, and is a usage error when the
// acknowledgement names another table. Either way nothing executes: the
// table's columns, constraints, indexes, and index storage are unchanged,
// the engine wrote no checkpoint, and the table still takes writes. A new
// registry entry fails here until it has a scenario.
func TestIrreversibleOperationsAreGated(t *testing.T) {
	// A server of its own pinned to wal_level=replica: the block-writes
	// entry is refused only where the route cannot stream, and a shared
	// server (PG_DSN, a hosted fixture) may run with logical decoding on.
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=replica")
	// The pool is sized for the route: a smaller one is refused before any
	// gate runs, as a failure rather than a verdict.
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL), MaxConnsFloor: CopySwapMinConns(copier.Options{})}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	scenarios := irreversibleScenarios()
	ops := irreversibleOps()
	keys := make(map[string]bool, len(ops))
	for _, op := range ops {
		keys[op.key] = true
	}
	for key := range scenarios {
		assert.True(t, keys[key], "scenario %q matches no registry entry", key)
	}

	for _, op := range ops {
		t.Run(op.key, func(t *testing.T) {
			sc, ok := scenarios[op.key]
			require.True(t, ok, "registry entry %q has no behaviour scenario", op.key)

			if op.refusal != "" {
				t.Run("refused without the acknowledgement", func(t *testing.T) {
					schema, before := newIrreversibleFixture(t, pool)
					table := schema + ".orders"
					v, err := Run(t.Context(), pool, parseIrreversible(t, sc.sql, schema), sc.options(cfg, table, ""))
					require.NoError(t, err, "a refusal is a verdict, not an error")
					assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
					assert.Equal(t, op.refusal, v.Reason)
					assert.Equal(t, sc.cause, v.Cause, "the refusal comes from the entry's own gate")
					assertNothingExecuted(t, pool, schema, before)
				})
			}

			t.Run("a usage error when the acknowledgement names another table", func(t *testing.T) {
				schema, before := newIrreversibleFixture(t, pool)
				table := schema + ".orders"
				v, err := Run(t.Context(), pool, parseIrreversible(t, sc.sql, schema), sc.options(cfg, table, schema+".other"))
				require.Error(t, err)
				assert.Equal(t, verdict.Verdict{}, v, "an error before a verdict carries a zero verdict")
				assertNothingExecuted(t, pool, schema, before)
			})
		})
	}
}

// newIrreversibleFixture creates the orders table in a schema of its own —
// a column, a CHECK constraint, a NOT NULL, and an index for each entry to
// discard, and rows a copy would carry — and returns the schema with the
// table's catalog state.
func newIrreversibleFixture(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	schema := testutil.NewSchema(t, pool)
	for _, sql := range []string{`
		CREATE TABLE {schema}.orders (
			id   bigint PRIMARY KEY,
			qty  integer NOT NULL CONSTRAINT orders_qty_positive CHECK (qty > 0),
			note text NOT NULL
		)`,
		`CREATE INDEX orders_qty_idx ON {schema}.orders (qty)`, `
		INSERT INTO {schema}.orders (id, qty, note)
		SELECT g, g, 'note ' || g
		FROM generate_series(1, 100) g`,
	} {
		_, err := pool.Exec(t.Context(), strings.ReplaceAll(sql, "{schema}", schema))
		require.NoError(t, err)
	}
	return schema, tableCatalogState(t, pool, schema)
}

func parseIrreversible(t *testing.T, sql, schema string) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne(strings.ReplaceAll(sql, "{schema}", schema))
	require.NoError(t, err)
	return st
}

// tableCatalogState renders the orders table's columns with their types
// and nullability, its constraints, and its indexes with their storage
// files, so a dropped, added, retyped, rebuilt, or swapped-in object
// changes it.
func tableCatalogState(t *testing.T, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	var state string
	require.NoError(t, pool.QueryRow(t.Context(), `
		WITH tbl AS (SELECT ($1 || '.orders')::regclass AS oid)
		SELECT concat_ws(E'\n',
			(SELECT string_agg(format('column %s %s notnull=%s', attname, format_type(atttypid, atttypmod), attnotnull), E'\n' ORDER BY attnum)
			 FROM pg_attribute, tbl
			 WHERE attrelid = tbl.oid AND attnum > 0 AND NOT attisdropped),
			(SELECT string_agg(format('constraint %s %s', conname, pg_get_constraintdef(c.oid)), E'\n' ORDER BY conname)
			 FROM pg_constraint c, tbl
			 WHERE conrelid = tbl.oid),
			(SELECT string_agg(format('index %s file=%s', i.relname, i.relfilenode), E'\n' ORDER BY i.relname)
			 FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid, tbl
			 WHERE x.indrelid = tbl.oid),
			format('table oid=%s', (SELECT oid FROM tbl)))`, schema).Scan(&state))
	return state
}

// assertNothingExecuted pins the oracle every refusal and usage error
// shares: the catalog state is what it was, no checkpoint row exists for
// the table, and an insert still succeeds, so no write fence stands.
func assertNothingExecuted(t *testing.T, pool *pgxpool.Pool, schema, before string) {
	t.Helper()
	assert.Equal(t, before, tableCatalogState(t, pool, schema), "the table's catalog is unchanged")

	var engineSchema bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, preflight.EngineSchema).Scan(&engineSchema))
	assert.False(t, engineSchema, "no copy-and-swap checkpoint was written")

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("roll back the probe insert: %v", err)
		}
	}()
	_, err = tx.Exec(t.Context(), fmt.Sprintf(
		`INSERT INTO %s.orders (id, qty, note) VALUES (1000, 1, 'probe')`, schema))
	assert.NoError(t, err, "the table still takes writes")
}
