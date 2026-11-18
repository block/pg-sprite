package loadproof_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/preflight"
)

// removeLeftoversOnCleanup registers a cleanup that removes what a run
// leaves behind in the database outside the workload's own schema: its
// checkpoint row, and its replication slot and publication when the run
// ended before the engine removed them. A leftover slot pins WAL on the
// server for as long as it exists, so a failed proof must not leave one.
// The slot and publication are looked for under the name the engine
// derives for the table as well as the names the row records, since a run
// killed after it made its slot and before it saved the row that names it
// leaves a slot no row knows. The whole cleanup is bounded by
// leftoversBudget. The workload's schema, with the shadow and retained
// source tables, is dropped by the table fixture's own cleanup.
func removeLeftoversOnCleanup(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable, release time.Duration) {
	t.Helper()
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), leftoversBudget(release))
		defer cancel()
		var database string
		if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
			t.Errorf("read the run's database: %v", err)
			return
		}
		derived := preflight.CopySwapDecodingName(database, table.Schema, table.Table)
		slots, publications := []string{derived}, []string{derived}
		cp, err := store.Inspect(ctx, table.Schema, table.Table)
		rowTable := true
		switch {
		case errors.Is(err, checkpoint.ErrTableMissing):
			rowTable = false
		case errors.Is(err, checkpoint.ErrNotFound):
		case err != nil:
			t.Errorf("inspect the run's checkpoint row: %v", err)
		default:
			slots = appendNamed(slots, cp.SlotName)
			publications = appendNamed(publications, cp.PublicationName)
		}
		for _, slot := range slots {
			dropped, err := dropSlotWhenReleased(ctx, pool, slot, release)
			if err != nil {
				t.Errorf("drop leftover replication slot %s: %v", slot, err)
			} else if dropped {
				t.Logf("dropped the replication slot %s the run left behind", slot)
			}
		}
		for _, publication := range publications {
			if _, err := pool.Exec(ctx, "DROP PUBLICATION IF EXISTS "+pgx.Identifier{publication}.Sanitize()); err != nil {
				t.Errorf("drop leftover publication %s: %v", publication, err)
			}
		}
		if !rowTable {
			return
		}
		if _, err := pool.Exec(ctx, deleteCheckpointSQL, table.Schema, table.Table); err != nil {
			t.Errorf("delete the run's checkpoint row: %v", err)
		}
	})
}

// appendNamed adds name to names unless it is empty or already there.
func appendNamed(names []string, name string) []string {
	if name == "" || slices.Contains(names, name) {
		return names
	}
	return append(names, name)
}

// deleteCheckpointSQL removes the run's checkpoint row. The harness owns
// the target outright, so unlike the engine's own delete it needs no
// identity match: whatever row the run left for the table goes.
const deleteCheckpointSQL = `DELETE FROM "` + checkpoint.SchemaName + `"."` + checkpoint.TableName + `"
	WHERE schema_name = $1 AND table_name = $2`

// The wait for a run's slot to be let go before the cleanup drops it.
const (
	// slotReleaseMargin is added to the server's wal_sender_timeout. The
	// binary has exited by the time the cleanup runs, and the server
	// releases the slot once the walsender serving the binary notices its
	// client is gone: a moment after a closed connection, and within
	// wal_sender_timeout for a client that vanished without closing it. A
	// slot still held after the timeout and the margin belongs to a
	// process that is still running.
	slotReleaseMargin = 30 * time.Second
	// defaultWALSenderTimeout stands in for a wal_sender_timeout of zero,
	// which turns the server's own check off: it is the server's default.
	defaultWALSenderTimeout = time.Minute
	// slotReleasePollInterval paces the reads of the slot's holder.
	slotReleasePollInterval = 100 * time.Millisecond
	// leftoverDropsBudget bounds the leftover cleanup's statements beyond
	// the wait for the slot: a few reads and drops.
	leftoverDropsBudget = time.Minute
)

// leftoversBudget bounds the whole leftover cleanup, the wait for one held
// slot included.
func leftoversBudget(release time.Duration) time.Duration {
	return release + leftoverDropsBudget
}

// slotReleaseDeadline reads the server's wal_sender_timeout and returns how
// long the cleanup waits for a held slot to be let go.
func slotReleaseDeadline(ctx context.Context, pool *pgxpool.Pool) (time.Duration, error) {
	var ms int64
	err := pool.QueryRow(ctx, `SELECT setting::bigint FROM pg_settings WHERE name = 'wal_sender_timeout'`).Scan(&ms)
	if err != nil {
		return 0, fmt.Errorf("read wal_sender_timeout: %w", err)
	}
	timeout := time.Duration(ms) * time.Millisecond
	if timeout == 0 {
		timeout = defaultWALSenderTimeout
	}
	return timeout + slotReleaseMargin, nil
}

// errSlotStillHeld reports a slot a backend still held when the wait for
// it to be let go ran out.
var errSlotStillHeld = errors.New("replication slot still held")

// dropSlotIfLeft drops the replication slot when it still exists, once no
// backend holds it, reporting whether it did. A slot the engine already
// removed is not an error. A slot still held is waited on for the
// server's slotReleaseDeadline, since the server lets go of a killed
// binary's slot only after it notices the closed connection; one held past
// that belongs to a process that is still running, and is left to it with
// errSlotStillHeld.
func dropSlotIfLeft(ctx context.Context, pool *pgxpool.Pool, slot string) (bool, error) {
	deadline, err := slotReleaseDeadline(ctx, pool)
	if err != nil {
		return false, err
	}
	return dropSlotWhenReleased(ctx, pool, slot, deadline)
}

// dropSlotWhenReleased is dropSlotIfLeft with the wait for the slot's
// holder bounded by deadline.
func dropSlotWhenReleased(ctx context.Context, pool *pgxpool.Pool, slot string, deadline time.Duration) (bool, error) {
	giveUp := time.NewTimer(deadline)
	defer giveUp.Stop()
	poll := time.NewTicker(slotReleasePollInterval)
	defer poll.Stop()
	for {
		holder, found, err := slotHolder(ctx, pool, slot)
		if err != nil {
			return false, fmt.Errorf("read replication slot %s: %w", slot, err)
		}
		if !found {
			return false, nil
		}
		if holder == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("wait for replication slot %s to be let go: %w", slot, ctx.Err())
		case <-giveUp.C:
			return false, fmt.Errorf("%w: backend %d still holds %s after %s, so the process that owns it is still running", errSlotStillHeld, holder, slot, deadline)
		case <-poll.C:
		}
	}
	if _, err := pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slot); err != nil {
		return false, fmt.Errorf("drop replication slot %s: %w", slot, err)
	}
	return true, nil
}

// slotHolder reads the process ID of the backend holding the slot, zero
// when none does, and whether the slot exists.
func slotHolder(ctx context.Context, pool *pgxpool.Pool, slot string) (holder int32, found bool, err error) {
	err = pool.QueryRow(ctx, `SELECT coalesce(active_pid, 0) FROM pg_replication_slots WHERE slot_name = $1`, slot).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return holder, true, nil
}
