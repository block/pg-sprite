package checkpoint_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// A target with no row is ErrNotFound: a completed read that found nothing,
// the one outcome a run may start fresh from.
func TestLoadReportsNotFoundForATargetWithNoRow(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	_, err := f.store.Load(t.Context(), "app", "orders", ordersCheckpoint().Fingerprints())
	assert.ErrorIs(t, err, checkpoint.ErrNotFound)
	var incompatible *checkpoint.IncompatibleError
	assert.False(t, errors.As(err, &incompatible))
}

// A row written for another statement — here a different after-schema
// digest — is the typed mismatch, never a Checkpoint and never ErrNotFound.
func TestLoadReportsAnotherStatementsRowAsIncompatible(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), ordersCheckpoint()))

	_, err := f.store.Load(t.Context(), "app", "orders", checkpoint.Fingerprints{Source: "src-a", Target: "tgt-after-edit"})
	var incompatible *checkpoint.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, checkpoint.MismatchTarget, incompatible.Mismatch)
	assert.Equal(t, "tgt-a", incompatible.Have)
	assert.Equal(t, "tgt-after-edit", incompatible.Want)
	assert.NotErrorIs(t, err, checkpoint.ErrNotFound)
}

// A row written in another row format is incompatible before its
// fingerprints are consulted, even though they match.
func TestLoadReportsAnotherFormatVersionAsIncompatible(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), ordersCheckpoint()))
	_, err := f.pool.Exec(t.Context(), "UPDATE pgsprite.pgsprite_checkpoint SET format_version = format_version + 1")
	require.NoError(t, err)

	_, err = f.store.Load(t.Context(), "app", "orders", ordersCheckpoint().Fingerprints())
	var incompatible *checkpoint.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, checkpoint.MismatchFormat, incompatible.Mismatch)
	assert.Equal(t, "2", incompatible.Have)
	assert.Equal(t, "1", incompatible.Want)
}

// A backend terminated from outside the session — what a failover looks
// like from the client — is a transient read error: Load retries on a fresh
// connection and returns the row, and the blip is never mistaken for a
// missing checkpoint (ST-2). The pool is held to one connection so the
// terminated one is the one the first read must use.
func TestLoadRetriesAcrossATerminatedBackend(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), ordersCheckpoint()))

	single, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: f.url, MaxConns: 1, MinConns: 1})
	require.NoError(t, err)
	t.Cleanup(single.Close)
	waits := 0
	store, err := checkpoint.NewStore(single, checkpoint.Options{
		LoadAttempts: 3,
		LoadBackoff:  time.Millisecond,
		Sleep: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	})
	require.NoError(t, err)

	var pid int
	require.NoError(t, single.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))
	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", pid).Scan(&terminated))
	require.True(t, terminated)
	const goneDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", pid).Scan(&alive))
		return !alive
	}, goneDeadline, 20*time.Millisecond, "the terminated backend should leave pg_stat_activity")

	got, err := store.Load(t.Context(), "app", "orders", ordersCheckpoint().Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(1000), got.Watermark)
	assert.Equal(t, 1, waits, "exactly one read failed on the dead connection before the retry succeeded")
}
