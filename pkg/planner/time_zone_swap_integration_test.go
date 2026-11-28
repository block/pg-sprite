package planner_test

import (
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// zonePools hands out one pool per session time zone on a single server,
// so each test runs its change in the zone it names. Pools live as long as
// the test that made the zonePools, so its subtests share them.
type zonePools struct {
	owner *testing.T
	base  string
	mu    sync.Mutex
	pools map[string]*pgxpool.Pool
}

func newZonePools(t *testing.T) *zonePools {
	return &zonePools{owner: t, base: testutil.StartPostgres(t), pools: map[string]*pgxpool.Pool{}}
}

func (z *zonePools) in(t *testing.T, zone string) *pgxpool.Pool {
	t.Helper()
	z.mu.Lock()
	defer z.mu.Unlock()
	if pool, ok := z.pools[zone]; ok {
		return pool
	}
	u, err := url.Parse(z.base)
	require.NoError(t, err)
	q := u.Query()
	q.Set("TimeZone", zone)
	u.RawQuery = q.Encode()
	pool, err := dbconn.NewPool(z.owner.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err)
	z.owner.Cleanup(pool.Close)
	z.pools[zone] = pool
	return pool
}

// swapOutcome is the planner's verdict on a change and what the server
// then did: whether it gave the table, and the named index, new storage,
// and whether it re-created any of the table's constraints, which is how
// it checks a foreign key again.
type swapOutcome struct {
	decision           planner.Decision
	rewrote            bool
	rebuiltIndex       bool
	rebuiltConstraints bool
}

// classifyAndRun creates setup in a throwaway schema, which every
// "$schema." in setup and alter names, classifies alter from the facts the
// front doors read off pool, runs it, and compares the filenodes of table
// and of index (when one is named) before and after.
func classifyAndRun(t *testing.T, pool *pgxpool.Pool, setup, alter, table, index string) swapOutcome {
	t.Helper()
	schema := testutil.NewSchema(t, pool)
	in := func(sql string) string {
		return strings.ReplaceAll(sql, "$schema.", pgx.Identifier{schema}.Sanitize()+".")
	}
	_, err := pool.Exec(t.Context(), in(setup))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
	require.NoError(t, err)
	facts := planner.FactsFrom(live)
	facts.SessionTimeZone, err = preflight.SessionTimeZone(t.Context(), pool)
	require.NoError(t, err)
	facts.KeyedColumns, err = preflight.KeyedColumns(t.Context(), pool, schema, table)
	require.NoError(t, err)
	plan, err := planner.Classify(in(alter), facts)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)

	tableName := pgx.Identifier{schema, table}.Sanitize()
	indexName := tableName
	if index != "" {
		indexName = pgx.Identifier{schema, index}.Sanitize()
	}
	const filenodes = "SELECT pg_relation_filenode($1::regclass)::int8, pg_relation_filenode($2::regclass)::int8"
	var before, after, indexBefore, indexAfter int64
	const constraints = "SELECT coalesce(array_agg(oid::int8 ORDER BY conname), '{}') FROM pg_constraint WHERE conrelid = $1::regclass"
	var constraintsBefore, constraintsAfter []int64
	require.NoError(t, pool.QueryRow(t.Context(), filenodes, tableName, indexName).Scan(&before, &indexBefore))
	require.NoError(t, pool.QueryRow(t.Context(), constraints, tableName).Scan(&constraintsBefore))
	_, err = pool.Exec(t.Context(), in(alter))
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(t.Context(), filenodes, tableName, indexName).Scan(&after, &indexAfter))
	require.NoError(t, pool.QueryRow(t.Context(), constraints, tableName).Scan(&constraintsAfter))

	out := swapOutcome{
		decision:           plan.Decisions[0],
		rewrote:            before != after,
		rebuiltIndex:       index != "" && indexBefore != indexAfter,
		rebuiltConstraints: !slices.Equal(constraintsBefore, constraintsAfter),
	}
	assert.False(t, out.decision.Unverified, "every fact was read")
	return out
}

// assertRelabel checks a native verdict against the server: the change
// kept the table's storage.
func assertRelabel(t *testing.T, out swapOutcome, zone string) {
	t.Helper()
	assert.Equal(t, planner.RouteNative, out.decision.Route, "reason %s", out.decision.Reason)
	assert.Equal(t, planner.ReasonBinaryCoercible, out.decision.Reason)
	assert.Equal(t, zone, out.decision.ReadsInTimeZone)
	assert.False(t, out.rewrote, "a native verdict must never rewrite the table")
}

// assertCopyAndSwap checks a rewrite verdict.
func assertCopyAndSwap(t *testing.T, out swapOutcome, zone string) {
	t.Helper()
	assert.Equal(t, planner.RouteCopyAndSwap, out.decision.Route, "reason %s", out.decision.Reason)
	assert.Equal(t, planner.ReasonTypeRewrite, out.decision.Reason)
	assert.Equal(t, zone, out.decision.ReadsInTimeZone)
}

// In a UTC session PostgreSQL keeps every stored value of a scalar column
// that changes between timestamp and timestamptz at an unspecified or the
// maximum precision, so with no index or foreign key on the column the
// planner relabels it and the server keeps the table's storage.
func TestTimeZoneSwapRelabelsAnUnkeyedColumnInUTC(t *testing.T) {
	pools := newZonePools(t)

	t.Run("timestamp to timestamptz", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "")
		assertRelabel(t, out, "UTC")
	})
	t.Run("timestamptz to timestamp", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamptz
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamp`, "events", "")
		assertRelabel(t, out, "UTC")
	})
	t.Run("timestamp to timestamptz in Etc/UTC", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "Etc/UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "")
		assertRelabel(t, out, "Etc/UTC")
	})
	t.Run("timestamp(3) to timestamptz, which removes the precision", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp(3)
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "")
		assertRelabel(t, out, "UTC")
	})
	t.Run("timestamp to timestamptz(6), the maximum precision", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz(6)`, "events", "")
		assertRelabel(t, out, "UTC")
	})
}

// Outside UTC every stored value moves by the zone's offset, a precision
// below the maximum rounds every value, and an array's elements are
// converted one by one, so each of these rewrites the table and the
// planner routes it to copy-and-swap.
func TestTimeZoneSwapRewritesOutsideTheRelabelRule(t *testing.T) {
	pools := newZonePools(t)

	t.Run("timestamp to timestamptz in America/New_York", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "America/New_York"), `
			CREATE TABLE $schema.events (
				seen_at timestamp
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "")
		assertCopyAndSwap(t, out, "America/New_York")
		assert.True(t, out.rewrote)
	})
	t.Run("timestamp(3) to timestamptz(3) in UTC", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp(3)
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz(3)`, "events", "")
		assertCopyAndSwap(t, out, "UTC")
		assert.True(t, out.rewrote)
	})
	t.Run("timestamp array to timestamptz array in UTC", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp[]
			);
			INSERT INTO $schema.events VALUES (ARRAY[now()::timestamp])`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz[]`, "events", "")
		assertCopyAndSwap(t, out, "UTC")
		assert.True(t, out.rewrote)
	})
	t.Run("timestamp to timestamptz in GMT", func(t *testing.T) {
		// The server relabels this one too; the planner's UTC set is
		// narrow and takes the slower safe route.
		out := classifyAndRun(t, pools.in(t, "GMT"), `
			CREATE TABLE $schema.events (
				seen_at timestamp
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "")
		assertCopyAndSwap(t, out, "GMT")
	})
}

// Even in UTC, PostgreSQL rebuilds every index on the column under the
// change's ACCESS EXCLUSIVE lock, since the index's operator class changes
// with the type, whether the index is a plain one or backs a primary key,
// unique, or exclusion constraint. A foreign key on the column is checked
// again with a scan, since its equality operator changes too. So a column
// any of them reads takes copy-and-swap, and the server confirms the
// rebuild the planner predicted.
func TestTimeZoneSwapOnAKeyedColumnTakesCopyAndSwapInUTC(t *testing.T) {
	pools := newZonePools(t)

	t.Run("plain index", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp
			);
			CREATE INDEX events_seen_at_idx ON $schema.events (seen_at);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "events_seen_at_idx")
		assertCopyAndSwap(t, out, "UTC")
		assert.True(t, out.rebuiltIndex)
	})
	t.Run("primary key", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp,
				CONSTRAINT events_pkey PRIMARY KEY (seen_at)
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "events_pkey")
		assertCopyAndSwap(t, out, "UTC")
		assert.True(t, out.rebuiltIndex)
	})
	t.Run("unique constraint over two columns", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				tenant_id bigint,
				seen_at timestamp,
				CONSTRAINT events_tenant_seen_key UNIQUE (tenant_id, seen_at)
			);
			INSERT INTO $schema.events VALUES (1, now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "events_tenant_seen_key")
		assertCopyAndSwap(t, out, "UTC")
		assert.True(t, out.rebuiltIndex)
	})
	t.Run("exclusion constraint", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				seen_at timestamp,
				CONSTRAINT events_seen_excl EXCLUDE USING btree (seen_at WITH =)
			);
			INSERT INTO $schema.events VALUES (now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "events_seen_excl")
		assertCopyAndSwap(t, out, "UTC")
		assert.True(t, out.rebuiltIndex)
	})
	t.Run("foreign key on the referencing column", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.days (
				day timestamp,
				CONSTRAINT days_pkey PRIMARY KEY (day)
			);
			CREATE TABLE $schema.events (
				seen_at timestamp,
				CONSTRAINT events_seen_at_fkey FOREIGN KEY (seen_at) REFERENCES $schema.days (day)
			);
			INSERT INTO $schema.days VALUES ('2026-11-28');
			INSERT INTO $schema.events VALUES ('2026-11-28')`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "")
		assertCopyAndSwap(t, out, "UTC")
		assert.True(t, out.rebuiltConstraints, "the server re-creates the foreign key, which checks every row again")
	})
	// An INCLUDE column has no operator class, and PostgreSQL can keep the
	// index when only such a column changes type. KeyedColumns reads every
	// indkey column without telling INCLUDE columns from keys, so the planner
	// routes the change to copy-and-swap anyway. That fails safe: the
	// rewrite costs time, never stored values.
	t.Run("INCLUDE column of an index", func(t *testing.T) {
		out := classifyAndRun(t, pools.in(t, "UTC"), `
			CREATE TABLE $schema.events (
				id bigint,
				seen_at timestamp
			);
			CREATE UNIQUE INDEX events_id_incl ON $schema.events (id) INCLUDE (seen_at);
			INSERT INTO $schema.events VALUES (1, now())`,
			`ALTER TABLE $schema.events ALTER COLUMN seen_at TYPE timestamptz`, "events", "events_id_incl")
		assertCopyAndSwap(t, out, "UTC")
	})
}

// KeyedColumns names a column a plain index reads as a key, in an
// expression, or in a predicate, on the table or on a partition below it,
// and leaves out a column nothing reads.
func TestKeyedColumnsReadsPlainIndexesOnTheTableAndItsPartitions(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), strings.ReplaceAll(`
		CREATE TABLE $schema.parent (
			k int,
			e text,
			p bool,
			part_only timestamp,
			free timestamp
		) PARTITION BY RANGE (k);
		CREATE TABLE $schema.child PARTITION OF $schema.parent FOR VALUES FROM (0) TO (10);
		CREATE INDEX ON $schema.parent (k);
		CREATE INDEX ON $schema.parent (lower(e));
		CREATE INDEX ON $schema.child (k) WHERE p;
		CREATE INDEX ON $schema.child (part_only)`, "$schema.", pgx.Identifier{schema}.Sanitize()+"."))
	require.NoError(t, err)

	keyed, err := preflight.KeyedColumns(t.Context(), pool, schema, "parent")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"k": true, "e": true, "p": true, "part_only": true}, keyed)

	zone, err := preflight.SessionTimeZone(t.Context(), pool)
	require.NoError(t, err)
	assert.NotEmpty(t, zone)
}

// KeyedColumns names the columns of a primary key, a unique constraint, a
// partition's own unique or exclusion constraint, including an expression
// and a predicate the exclusion constraint reads, a constraint taken over
// from an existing index, and the referencing columns of a foreign key. PostgreSQL
// records each of those reads against the constraint rather than the
// index, and leaves out a column nothing reads.
func TestKeyedColumnsReadsConstraintsAndForeignKeys(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), strings.ReplaceAll(`
		CREATE TABLE $schema.days (
			day timestamp,
			CONSTRAINT days_pkey PRIMARY KEY (day)
		);
		CREATE TABLE $schema.parent (
			k int,
			pk_col int,
			uq_col timestamp,
			ex_expr text,
			ex_pred bool,
			adopted timestamp,
			part_uq timestamp,
			fk_col timestamp,
			free timestamp,
			CONSTRAINT parent_pkey PRIMARY KEY (k, pk_col),
			CONSTRAINT parent_uq_key UNIQUE (k, uq_col),
			CONSTRAINT parent_fk_col_fkey FOREIGN KEY (fk_col) REFERENCES $schema.days (day)
		) PARTITION BY RANGE (k);
		CREATE TABLE $schema.child PARTITION OF $schema.parent FOR VALUES FROM (0) TO (10);
		ALTER TABLE $schema.child ADD CONSTRAINT child_part_uq_key UNIQUE (part_uq);
		ALTER TABLE $schema.child ADD CONSTRAINT child_ex_excl EXCLUDE USING btree (lower(ex_expr) WITH =) WHERE (ex_pred);
		CREATE UNIQUE INDEX child_adopted_idx ON $schema.child (adopted);
		ALTER TABLE $schema.child ADD CONSTRAINT child_adopted_key UNIQUE USING INDEX child_adopted_idx`,
		"$schema.", pgx.Identifier{schema}.Sanitize()+"."))
	require.NoError(t, err)

	keyed, err := preflight.KeyedColumns(t.Context(), pool, schema, "parent")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"k": true, "pk_col": true, "uq_col": true, "ex_expr": true, "ex_pred": true,
		"adopted": true, "part_uq": true, "fk_col": true,
	}, keyed)
}
