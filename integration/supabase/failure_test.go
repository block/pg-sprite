package supabase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdditionalRewriteRefusals(t *testing.T) {
	pool := fixture(t)
	for _, tc := range []struct{ name, ddl string }{
		{"stored_generated", "ADD COLUMN doubled int GENERATED ALWAYS AS (id*2) STORED"},
		{"using_expression", "ALTER COLUMN body TYPE text USING upper(body)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := seedDDLTable(t, pool, "pgsprite_extra_"+tc.name)
			before := snapshot(t, pool, table)
			st, err := statement.ParseOne("ALTER TABLE " + table + " " + tc.ddl)
			require.NoError(t, err)
			v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonBackendUnavailable, v.Reason)
			assert.Equal(t, before, snapshot(t, pool, table))
		})
	}
}

func TestDesiredDestructivePlanDoesNotApplySafePrefix(t *testing.T) {
	pool := fixture(t)
	name := "pgsprite_destructive_plan"
	table := seedDDLTable(t, pool, name)
	before := snapshot(t, pool, table)
	ds, err := statement.ParseDesired("CREATE TABLE " + name + " (id int PRIMARY KEY,owner_id uuid NOT NULL,body text NOT NULL,label varchar(8),amount numeric(8,2),safe_prefix text)")
	require.NoError(t, err)
	result, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: "public", Desired: ds}, migrate.DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, result.Outcome)
	assert.Equal(t, verdict.ReasonDestructiveChange, result.Reason)
	assert.Empty(t, result.Verdicts)
	assert.Equal(t, before, snapshot(t, pool, table))
}

func TestSupabaseLockBudget(t *testing.T) {
	pool := fixture(t)
	table := seedDDLTable(t, pool, "pgsprite_lock_budget")
	before := snapshot(t, pool, table)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		err := tx.Rollback(ctx)
		if !errors.Is(err, pgx.ErrTxClosed) {
			assert.NoError(t, err)
		}
	})
	_, err = tx.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS SHARE MODE")
	require.NoError(t, err)
	st, err := statement.ParseOne("ALTER TABLE " + table + " ADD COLUMN blocked text")
	require.NoError(t, err)
	opts := migrate.DefaultOptions()
	opts.Budget.Brief.LockTimeout = 100 * time.Millisecond
	opts.Retry.MaxAttempts = 1
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	v, err := migrate.Run(ctx, pool, st, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
	assert.Equal(t, before, snapshot(t, pool, table))
	require.NoError(t, tx.Rollback(t.Context()))
	verifyAPITenants(t, "pgsprite_lock_budget", map[int][]int{1: {1}, 2: {2}, 3: {}})
}

func TestSupabaseNotNullValidationFailure(t *testing.T) {
	pool := fixture(t)
	table := seedDDLTable(t, pool, "pgsprite_bad_not_null")
	execSQL(t, pool, "UPDATE "+table+" SET note=NULL WHERE id=1")
	before := protections(t, pool, table)
	st, err := statement.ParseOne("ALTER TABLE " + table + " ALTER COLUMN note SET NOT NULL")
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
	require.Len(t, v.ExecutedSQL, 1, "the NOT VALID scaffold committed before validation failed")
	var notNull bool
	var nulls, scaffolds int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT attnotnull FROM pg_attribute WHERE attrelid=$1::regclass AND attname='note'", table).Scan(&notNull))
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE note IS NULL").Scan(&nulls))
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_constraint WHERE conrelid=$1::regclass AND contype='c' AND NOT convalidated", table).Scan(&scaffolds))
	assert.False(t, notNull)
	assert.Equal(t, 1, nulls)
	assert.Equal(t, 1, scaffolds)
	assert.Equal(t, before, protections(t, pool, table))
	verifyAPITenants(t, "pgsprite_bad_not_null", map[int][]int{1: {1}, 2: {2}, 3: {}})
}

func TestSupabaseUniqueIndexFailure(t *testing.T) {
	pool := fixture(t)
	table := seedDDLTable(t, pool, "pgsprite_duplicate_index")
	execSQL(t, pool, "UPDATE "+table+" SET body='duplicate'")
	before := protections(t, pool, table)
	st, err := statement.ParseOne("CREATE UNIQUE INDEX pgsprite_duplicate_body ON " + table + "(body)")
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
	require.ErrorIs(t, err, executor.ErrBuildLeftInvalidIndex)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeInvalidIndexOwnLeftover), v.Code)
	var valid bool
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT indisvalid FROM pg_index WHERE indexrelid='public.pgsprite_duplicate_body'::regclass").Scan(&valid))
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE body='duplicate'").Scan(&count))
	assert.False(t, valid)
	assert.Equal(t, 2, count)
	assert.Equal(t, before, protections(t, pool, table))
	verifyAPITenants(t, "pgsprite_duplicate_index", map[int][]int{1: {1}, 2: {2}, 3: {}})
}

func TestForeignKeyToSupabaseAuth(t *testing.T) {
	pool := fixture(t)
	execSQL(t, pool, "INSERT INTO auth.users(id) VALUES ('00000000-0000-0000-0000-000000000001'),('00000000-0000-0000-0000-000000000002')")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DELETE FROM auth.users WHERE id IN ('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002')")
		assert.NoError(t, err)
	})
	table := seedDDLTable(t, pool, "pgsprite_auth_fk")
	before := protections(t, pool, table)
	v := change(t, pool, "ALTER TABLE "+table+" ADD CONSTRAINT owner_fk FOREIGN KEY(owner_id) REFERENCES auth.users(id)")
	require.Len(t, v.ExecutedSQL, 2)
	var valid bool
	var target string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT convalidated,confrelid::regclass::text FROM pg_constraint WHERE conrelid=$1::regclass AND conname='owner_fk'", table).Scan(&valid, &target))
	assert.True(t, valid)
	assert.Equal(t, "auth.users", target)
	assert.Equal(t, before, protections(t, pool, table))
	_, err := pool.Exec(t.Context(), "INSERT INTO "+table+"(id,owner_id,body) VALUES(3,'00000000-0000-0000-0000-000000000003','orphan')")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23503", pgErr.Code)
	verifyAPITenants(t, "pgsprite_auth_fk", map[int][]int{1: {1}, 2: {2}, 3: {}})
}
