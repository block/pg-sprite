package preflight_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// probeDeadline bounds one proof, which never waits on a lock and so
// returns at once.
const probeDeadline = 10 * time.Second

// rewriteFixture is a schema holding table t, with 100 rows and the
// columns id and note, beside three domains: positive_int, a CHECK;
// required_text, a NOT NULL; and plain_text, no constraint at all.
type rewriteFixture struct {
	url    string
	pool   *pgxpool.Pool
	schema string
}

func newRewriteFixture(t *testing.T) rewriteFixture {
	t.Helper()
	dsn := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := rewriteFixture{url: dsn, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `CREATE DOMAIN %[1]s.positive_int AS int CHECK (VALUE > 0)`)
	f.exec(t, `CREATE DOMAIN %[1]s.required_text AS text NOT NULL`)
	f.exec(t, `CREATE DOMAIN %[1]s.plain_text AS text`)
	f.exec(t, `
		CREATE TABLE %[1]s.t (
			id   bigint PRIMARY KEY,
			note text
		)`)
	f.exec(t, `INSERT INTO %[1]s.t SELECT g, 'n' FROM generate_series(1, 100) g`)
	return f
}

// exec runs ddl with the fixture's schema substituted for %[1]s.
func (f rewriteFixture) exec(t *testing.T, ddl string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(ddl, f.schema))
	require.NoError(t, err, ddl)
}

// holdAccessExclusive locks t from another session until the test ends,
// so a proof that opened the live table would wait and fail the deadline.
func (f rewriteFixture) holdAccessExclusive(t *testing.T) {
	t.Helper()
	holder, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) })
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", f.schema))
	require.NoError(t, err)
}

// prove runs the proof for alter, with the fixture's schema substituted
// for %[1]s, and fails the test if it waited on a lock.
func (f rewriteFixture) prove(t *testing.T, alter string) map[string]statement.AddColumnProof {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), probeDeadline)
	defer cancel()
	start := time.Now()
	proofs, err := preflight.AddColumnRewrites(ctx, f.pool, f.schema, "t", fmt.Sprintf(alter, f.schema))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), probeDeadline/2, "a probe waited on a lock")
	return proofs
}

// The server, not the statement's shape, decides whether adding a column
// rewrites the table, and a rewrite carries its cause: the column's type
// or its default. Every proof runs while another session holds ACCESS
// EXCLUSIVE on the live table, which the proof never opens.
func TestAddColumnRewritesAsTheServerDecides(t *testing.T) {
	f := newRewriteFixture(t)
	f.holdAccessExclusive(t)

	cases := []struct {
		name  string
		alter string
		want  statement.AddColumnProof
		why   string
	}{
		{
			name:  "plain column",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN plain int`,
			want:  statement.AddColumnInPlace,
			why:   "a nullable column with no default is a catalog entry",
		},
		{
			name:  "constant default with NOT NULL",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN constant int NOT NULL DEFAULT 0`,
			want:  statement.AddColumnInPlace,
			why:   "a constant default is stored in the catalog",
		},
		{
			name:  "stable default now()",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN stamped timestamptz NOT NULL DEFAULT now()`,
			want:  statement.AddColumnInPlace,
			why:   "a non-volatile default is evaluated once and stored like a constant",
		},
		{
			name:  "stable default current_user",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN who text DEFAULT current_user`,
			want:  statement.AddColumnInPlace,
			why:   "a non-volatile default is evaluated once and stored like a constant",
		},
		{
			name:  "array constant default",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN tags text[] DEFAULT '{}'`,
			want:  statement.AddColumnInPlace,
			why:   "a constant default is stored in the catalog",
		},
		{
			name:  "constant default with an inline check",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN checked int DEFAULT 1 CHECK (checked > 0)`,
			want:  statement.AddColumnInPlace,
			why:   "an inline CHECK scans the table but never rewrites it, so the probe leaves it out",
		},
		{
			name:  "domain without constraints",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN plain_domain %[1]s.plain_text DEFAULT 'x'`,
			want:  statement.AddColumnInPlace,
			why:   "a domain with nothing to check behaves as its base type",
		},
		{
			name:  "check domain without a default",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN bare_domain %[1]s.positive_int`,
			want:  statement.AddColumnTypeRewrites,
			why:   "the domain's check runs against every row, even for a NULL",
		},
		{
			name:  "check domain with a constant default",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN domain_default %[1]s.positive_int DEFAULT 1`,
			want:  statement.AddColumnTypeRewrites,
			why:   "the domain's check runs against every row whatever the default",
		},
		{
			name:  "check domain with a stable default",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN domain_stable %[1]s.positive_int DEFAULT length(current_user)`,
			want:  statement.AddColumnTypeRewrites,
			why:   "the default alone would be stored; the domain is what rewrites",
		},
		{
			name:  "NOT NULL domain with a constant default",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN required %[1]s.required_text DEFAULT 'r'`,
			want:  statement.AddColumnTypeRewrites,
			why:   "a domain's NOT NULL is a constraint checked against every row",
		},
		{
			name:  "volatile default gen_random_uuid()",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN token uuid DEFAULT gen_random_uuid()`,
			want:  statement.AddColumnDefaultRewrites,
			why:   "a volatile default is computed for every row",
		},
		{
			name:  "volatile default clock_timestamp()",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN started timestamptz DEFAULT clock_timestamp()`,
			want:  statement.AddColumnDefaultRewrites,
			why:   "a volatile default is computed for every row",
		},
		{
			name:  "bigserial",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN seq bigserial`,
			want:  statement.AddColumnDefaultRewrites,
			why:   "serial is bigint with a nextval() default, which is volatile",
		},
		{
			name:  "identity",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN ident bigint GENERATED ALWAYS AS IDENTITY`,
			want:  statement.AddColumnDefaultRewrites,
			why:   "an identity draws every row's value from its sequence",
		},
		{
			name:  "unqualified type in the statement's schema",
			alter: `ALTER TABLE %[1]s.t ADD COLUMN bare positive_int DEFAULT 1`,
			want:  statement.AddColumnTypeRewrites,
			why:   "an unqualified type resolves in the statement's schema, as the native executor resolves it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proofs := f.prove(t, tc.alter)
			require.Len(t, proofs, 1)
			for column, proof := range proofs {
				assert.Equal(t, tc.want, proof, "column %s: %s", column, tc.why)
			}
		})
	}
}

// A statement adding several columns is proved one column at a time, and
// a stored generated column gets no proof: the planner routes it by its
// generation alone. The live table is left exactly as it was.
func TestAddColumnRewritesProvesEachColumnOfOneStatement(t *testing.T) {
	f := newRewriteFixture(t)
	var filenodeBefore uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT pg_relation_filenode($1::regclass)", f.schema+".t").Scan(&filenodeBefore))
	f.holdAccessExclusive(t)

	proofs := f.prove(t, `
		ALTER TABLE %[1]s.t
			ADD COLUMN plain int,
			ADD COLUMN domain_default %[1]s.positive_int DEFAULT 1,
			ADD COLUMN token uuid DEFAULT gen_random_uuid(),
			ADD COLUMN doubled bigint GENERATED ALWAYS AS (id * 2) STORED`)
	assert.Equal(t, map[string]statement.AddColumnProof{
		"plain":          statement.AddColumnInPlace,
		"domain_default": statement.AddColumnTypeRewrites,
		"token":          statement.AddColumnDefaultRewrites,
	}, proofs)

	var filenodeAfter uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT pg_relation_filenode($1::regclass)", f.schema+".t").Scan(&filenodeAfter))
	assert.Equal(t, filenodeBefore, filenodeAfter)
	var columns int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_attribute WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped",
		f.schema+".t").Scan(&columns))
	assert.Equal(t, 2, columns)
}

// A probe the server rejects is an error naming the column, never an answer
// either way.
func TestAddColumnRewritesSurfacesARejectedProbe(t *testing.T) {
	f := newRewriteFixture(t)

	_, err := preflight.AddColumnRewrites(t.Context(), f.pool, f.schema, "t",
		fmt.Sprintf(`ALTER TABLE %[1]s.t ADD COLUMN c %[1]s.no_such_type`, f.schema))
	require.Error(t, err)
	assert.NotErrorIs(t, err, preflight.ErrAddColumnProbeUnavailable)
	assert.Contains(t, err.Error(), `add column "c" of `)
	assert.Contains(t, err.Error(), `to the probe table`)
}

// A server that accepts no writes, as a hot standby does, cannot create
// the probe's temporary table, so whether the column rewrites the table
// is unknown. The proof fails with the typed error naming the fix rather
// than answering either way. A read-only session stands in for the
// standby: both refuse the CREATE with read_only_sql_transaction.
func TestAddColumnRewritesOnAServerThatAcceptsNoWrites(t *testing.T) {
	f := newRewriteFixture(t)
	dsn, err := url.Parse(f.url)
	require.NoError(t, err)
	query := dsn.Query()
	query.Set("default_transaction_read_only", "on")
	dsn.RawQuery = query.Encode()
	readOnly, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: dsn.String()})
	require.NoError(t, err)
	t.Cleanup(readOnly.Close)

	_, err = preflight.AddColumnRewrites(t.Context(), readOnly, f.schema, "t",
		fmt.Sprintf(`ALTER TABLE %[1]s.t ADD COLUMN plain int`, f.schema))
	require.ErrorIs(t, err, preflight.ErrAddColumnProbeUnavailable)
	assert.Contains(t, err.Error(), "plan the change against the primary")
}

// A default, or a domain check, that calls a function reading the live
// table is evaluated by the probe the way the real ALTER evaluates it, so
// it needs a lock another session may hold. The probe never waits for it:
// the column is unproven, which the planner keeps off the native path, and
// the proof returns at once.
func TestAddColumnRewritesNeverWaitsOnALock(t *testing.T) {
	f := newRewriteFixture(t)
	f.exec(t, `
		CREATE FUNCTION %[1]s.max_id() RETURNS bigint
			STABLE LANGUAGE sql
			AS 'SELECT coalesce(max(id), 0) FROM %[1]s.t'`)
	f.exec(t, `
		CREATE FUNCTION %[1]s.below_max(v bigint) RETURNS boolean
			STABLE LANGUAGE sql
			AS 'SELECT v <= (SELECT coalesce(max(id), 0) + 10 FROM %[1]s.t)'`)
	f.exec(t, `CREATE DOMAIN %[1]s.bounded AS bigint CHECK (%[1]s.below_max(VALUE))`)
	f.holdAccessExclusive(t)

	t.Run("default reading the locked table", func(t *testing.T) {
		proofs := f.prove(t, `ALTER TABLE %[1]s.t ADD COLUMN high bigint DEFAULT %[1]s.max_id()`)
		assert.Equal(t, map[string]statement.AddColumnProof{"high": statement.AddColumnUnproven}, proofs)
	})
	t.Run("domain check reading the locked table", func(t *testing.T) {
		proofs := f.prove(t, `ALTER TABLE %[1]s.t ADD COLUMN capped %[1]s.bounded DEFAULT 5`)
		assert.Equal(t, map[string]statement.AddColumnProof{"capped": statement.AddColumnUnproven}, proofs)
	})
}

// An identity's SEQUENCE NAME in a permanent schema is left out of the
// probe, so the proof creates no sequence there.
func TestAddColumnRewritesCreatesNoSequenceInTheLiveSchema(t *testing.T) {
	f := newRewriteFixture(t)

	proofs := f.prove(t, `
		ALTER TABLE %[1]s.t
			ADD COLUMN ident bigint GENERATED ALWAYS AS IDENTITY (SEQUENCE NAME %[1]s.ident_seq)`)
	assert.Equal(t, map[string]statement.AddColumnProof{"ident": statement.AddColumnDefaultRewrites}, proofs)

	var sequences int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind = 'S'",
		f.schema).Scan(&sequences))
	assert.Zero(t, sequences, "the probe created a sequence in the live schema")
}

// An ADD COLUMN IF NOT EXISTS for a column the table already has is
// skipped by the server, so it gets no proof; one for a new column is
// proved like any other.
func TestAddColumnRewritesSkipsAColumnTheTableHas(t *testing.T) {
	f := newRewriteFixture(t)

	proofs := f.prove(t, `
		ALTER TABLE %[1]s.t
			ADD COLUMN IF NOT EXISTS note text DEFAULT 'x',
			ADD COLUMN IF NOT EXISTS fresh text DEFAULT 'x'`)
	assert.Equal(t, map[string]statement.AddColumnProof{"fresh": statement.AddColumnInPlace}, proofs)
}
