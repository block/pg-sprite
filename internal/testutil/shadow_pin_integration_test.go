package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pin whose insert fails — here because pg_catalog has no such shadow,
// on any server — rolls its transaction back as it fails, so its
// connection is back in the pool before anyone ends the pin, and the
// cleanup reports the failure with the server's undefined_table SQLSTATE
// and no transaction to end.
func TestShadowPinRollsBackAFailedInsert(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), StartPostgres(t))
	require.NoError(t, err)
	pin := newShadowPin(t.Context(), pool, pgx.Identifier{"pg_catalog", "missing_shadow"}, 1, 0)

	pin.Clock().Now()
	// A connection the failed pin still held would make pool.Close wait on
	// it forever, so the pool is closed only once none is acquired.
	require.Zero(t, pool.Stat().AcquiredConns(), "the failed pin's connection is back in the pool")
	pool.Close()

	assert.Nil(t, pin.tx, "a failed insert leaves no pin to release")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, pin.end(context.WithoutCancel(t.Context())), &pgErr)
	assert.Equal(t, "42P01", pgErr.Code, "undefined_table: the shadow does not exist")
}
