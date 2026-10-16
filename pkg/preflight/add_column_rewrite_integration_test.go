package preflight_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The server, not the statement's shape, decides whether adding a column
// rewrites the table: a constant default on a domain with constraints
// rewrites every row, and now() is stored in the catalog like a constant.
// The proof never opens the live table: it completes while another session
// holds ACCESS EXCLUSIVE on it, and leaves it exactly as it was.
func TestAddColumnRewritesAsTheServerDecides(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE DOMAIN %[1]s.positive_int AS int CHECK (VALUE > 0)",
		"CREATE DOMAIN %[1]s.required_text AS text NOT NULL",
		"CREATE DOMAIN %[1]s.plain_text AS text",
		"CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, note text)",
		"INSERT INTO %[1]s.t SELECT g, 'n' FROM generate_series(1, 100) g",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}

	var filenodeBefore uint32
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_relation_filenode($1::regclass)", schema+".t").Scan(&filenodeBefore))

	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) })
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rewrites, err := preflight.AddColumnRewrites(ctx, pool, schema, "t", fmt.Sprintf(
		"ALTER TABLE %[1]s.t "+
			"ADD COLUMN plain int, "+
			"ADD COLUMN constant int NOT NULL DEFAULT 0, "+
			"ADD COLUMN stamped timestamptz NOT NULL DEFAULT now(), "+
			"ADD COLUMN who text DEFAULT current_user, "+
			"ADD COLUMN tags text[] DEFAULT '{}', "+
			"ADD COLUMN checked int DEFAULT 1 CHECK (checked > 0), "+
			"ADD COLUMN plain_domain %[1]s.plain_text DEFAULT 'x', "+
			"ADD COLUMN bare_domain %[1]s.positive_int, "+
			"ADD COLUMN domain_default %[1]s.positive_int DEFAULT 1, "+
			"ADD COLUMN required %[1]s.required_text DEFAULT 'r', "+
			"ADD COLUMN token uuid DEFAULT gen_random_uuid(), "+
			"ADD COLUMN started timestamptz DEFAULT clock_timestamp(), "+
			"ADD COLUMN seq bigserial, "+
			"ADD COLUMN ident bigint GENERATED ALWAYS AS IDENTITY, "+
			"ADD COLUMN doubled bigint GENERATED ALWAYS AS (id * 2) STORED", schema))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"plain":          false,
		"constant":       false,
		"stamped":        false,
		"who":            false,
		"tags":           false,
		"checked":        false,
		"plain_domain":   false,
		"bare_domain":    true,
		"domain_default": true,
		"required":       true,
		"token":          true,
		"started":        true,
		"seq":            true,
		"ident":          true,
	}, rewrites)

	// An unqualified type resolves in the statement's schema, as it does
	// when the native executor runs the statement.
	rewrites, err = preflight.AddColumnRewrites(ctx, pool, schema, "t",
		fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN bare positive_int DEFAULT 1", schema))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"bare": true}, rewrites)
	require.NoError(t, holder.Rollback(t.Context()))

	var filenodeAfter uint32
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_relation_filenode($1::regclass)", schema+".t").Scan(&filenodeAfter))
	assert.Equal(t, filenodeBefore, filenodeAfter)
	var columns int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_attribute WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped",
		schema+".t").Scan(&columns))
	assert.Equal(t, 2, columns)
}

// A probe the server rejects is an error naming the column, never an answer
// either way.
func TestAddColumnRewritesSurfacesARejectedProbe(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id bigint PRIMARY KEY)", schema))
	require.NoError(t, err)

	_, err = preflight.AddColumnRewrites(t.Context(), pool, schema, "t",
		fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN c %s.no_such_type", schema, schema))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `add column "c" of `)
	assert.Contains(t, err.Error(), `to the probe table`)
}

// A default, or a domain check, that calls a function reading the live
// table is evaluated by the probe the way the real ALTER evaluates it, so
// it needs a lock another session may hold. The probe never waits for it:
// the column is reported as rewriting, which keeps the change off the
// native path, and the proof returns at once. An identity's SEQUENCE NAME
// in a permanent schema is left out of the probe, so no sequence is
// created there, and an IF NOT EXISTS for a column the table already has
// is skipped by the server and gets no proof.
func TestAddColumnRewritesNeverWaitsOnALock(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, note text)",
		"CREATE FUNCTION %[1]s.max_id() RETURNS bigint STABLE LANGUAGE sql AS 'SELECT coalesce(max(id), 0) FROM %[1]s.t'",
		"CREATE FUNCTION %[1]s.below_max(v bigint) RETURNS boolean STABLE LANGUAGE sql AS 'SELECT v <= (SELECT coalesce(max(id), 0) + 10 FROM %[1]s.t)'",
		"CREATE DOMAIN %[1]s.bounded AS bigint CHECK (%[1]s.below_max(VALUE))",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}

	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) })
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	rewrites, err := preflight.AddColumnRewrites(ctx, pool, schema, "t", fmt.Sprintf(
		"ALTER TABLE %[1]s.t "+
			"ADD COLUMN high bigint DEFAULT %[1]s.max_id(), "+
			"ADD COLUMN capped %[1]s.bounded DEFAULT 5, "+
			"ADD COLUMN ident bigint GENERATED ALWAYS AS IDENTITY (SEQUENCE NAME %[1]s.ident_seq), "+
			"ADD COLUMN IF NOT EXISTS note text DEFAULT 'x', "+
			"ADD COLUMN IF NOT EXISTS fresh text DEFAULT 'x'", schema))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "a probe waited on the held lock")
	assert.Equal(t, map[string]bool{
		"high":   true,
		"capped": true,
		"ident":  true,
		"fresh":  false,
	}, rewrites)
	require.NoError(t, holder.Rollback(t.Context()))

	var sequences int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind = 'S'",
		schema).Scan(&sequences))
	assert.Zero(t, sequences, "the probe created a sequence in the live schema")
}
