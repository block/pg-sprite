package loadproof_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// removeLeftoversOnCleanup registers a cleanup that removes what a run
// leaves behind in the database outside the workload's own schema: its
// checkpoint row, and the replication slot and publication the row names
// when the run ended before the engine removed them. A leftover slot pins
// WAL on the server for as long as it exists, so a failed proof must not
// leave one. The workload's schema, with the shadow and retained source
// tables, is dropped by the table fixture's own cleanup.
func removeLeftoversOnCleanup(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable) {
	t.Helper()
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		cp, err := store.Inspect(ctx, table.Schema, table.Table)
		if errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrTableMissing) {
			return
		}
		if err != nil {
			t.Errorf("inspect the run's checkpoint row: %v", err)
			return
		}
		if cp.SlotName != "" {
			dropped, err := dropSlotIfLeft(ctx, pool, cp.SlotName)
			if err != nil {
				t.Errorf("drop leftover replication slot %s: %v", cp.SlotName, err)
			} else if dropped {
				t.Logf("dropped the replication slot %s the run left behind", cp.SlotName)
			}
		}
		if cp.PublicationName != "" {
			if _, err := pool.Exec(ctx, "DROP PUBLICATION IF EXISTS "+pgx.Identifier{cp.PublicationName}.Sanitize()); err != nil {
				t.Errorf("drop leftover publication %s: %v", cp.PublicationName, err)
			}
		}
		if _, err := pool.Exec(ctx, deleteCheckpointSQL, table.Schema, table.Table); err != nil {
			t.Errorf("delete the run's checkpoint row: %v", err)
		}
	})
}

// deleteCheckpointSQL removes the run's checkpoint row. The harness owns
// the target outright, so unlike the engine's own delete it needs no
// identity match: whatever row the run left for the table goes.
const deleteCheckpointSQL = `DELETE FROM "` + checkpoint.SchemaName + `"."` + checkpoint.TableName + `"
	WHERE schema_name = $1 AND table_name = $2`

// dropSlotIfLeft drops the replication slot when it still exists and no
// backend holds it, reporting whether it did. A slot the engine already
// removed is not an error; a slot still active belongs to a process that
// is still running and is left to it.
func dropSlotIfLeft(ctx context.Context, pool *pgxpool.Pool, slot string) (bool, error) {
	var active bool
	err := pool.QueryRow(ctx, `SELECT active FROM pg_replication_slots WHERE slot_name = $1`, slot).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if active {
		return false, errors.New("slot is still active")
	}
	if _, err := pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slot); err != nil {
		return false, err
	}
	return true, nil
}
