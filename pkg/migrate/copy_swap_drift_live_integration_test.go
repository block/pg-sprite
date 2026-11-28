package migrate_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A live run of
//
//	ALTER TABLE workload ALTER COLUMN amount TYPE numeric(14,4)
//
// on the live fixture's workload table, during whose copy someone changes
// the source by hand:
//
//	ALTER TABLE workload ADD COLUMN hand_note text;
//	CREATE INDEX <workload>_hand_label_idx ON workload (label);
//
// The copy, the catch-up, and the verification pass carry only the columns
// the shadow was built with, so the run reaches its cutover gate, which
// re-fingerprints the source and refuses as cutover-schema-drift. The stop
// leaves the shadow, the replication slot — which keeps WAL on the server
// for as long as it stands — its publication, and the checkpoint row in
// verifying, and the detail names each of them and the two commands past
// them. Starting over with the restart acknowledgement discards all four
// and builds a fresh shadow from the source as it is now, so the change
// completes, keeps the column and the index added by hand, and drops the
// slot.
func TestRunCopySwapCutoverDriftNamesWhatTheLiveRunLeaves(t *testing.T) {
	f := newLiveFixture(t, testutil.WorkloadShape{})
	st := parseOne(t, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN amount TYPE numeric(14,4)`, f.table.Qualified()))
	table := f.table.Schema + "." + f.table.Table
	handIndex := pgx.Identifier{f.table.Table + "_hand_label_idx"}.Sanitize()

	opts := f.options()
	var changeByHand sync.Once
	opts.CopySwap.OnCheckpoint = func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying {
			return nil
		}
		var err error
		changeByHand.Do(func() {
			_, err = f.pool.Exec(ctx, fmt.Sprintf(`
				ALTER TABLE %s
					ADD COLUMN hand_note text`, f.table.Qualified()))
			if err != nil {
				return
			}
			_, err = f.pool.Exec(ctx, fmt.Sprintf(`
				CREATE INDEX %s
					ON %s (label)`, handIndex, f.table.Qualified()))
		})
		return err
	}
	v, err := migrate.Run(t.Context(), f.pool, st, opts)
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, verdict.Cause(schemachange.CauseSchemaDrift), v.Cause)

	row := f.inspectRow(t)
	require.Equal(t, checkpoint.PhaseVerifying, row.Phase, "the stop left the row where its last save put it")
	require.NotEmpty(t, row.SlotName)
	assert.Contains(t, v.Detail, "shadow table "+f.table.Schema+"."+schemachange.ShadowName(f.table.Schema, f.table.Table))
	assert.Contains(t, v.Detail, "replication slot "+row.SlotName+", which retains WAL on the server until it is dropped")
	assert.Contains(t, v.Detail, "publication "+row.PublicationName)
	assert.Contains(t, v.Detail, "the checkpoint row in phase verifying")
	assert.Contains(t, v.Detail, "--restart "+table)
	assert.Contains(t, v.Detail, "pg-sprite discard "+table)
	assert.NotContains(t, v.Detail, "RunOptions", "the detail names the CLI's commands, not the library's options")
	assert.True(t, f.slotExists(t, row.SlotName), "the stopped run's slot stands")

	restart := f.options()
	restart.CopySwap.Restart = table
	v, err = migrate.Run(t.Context(), f.pool, st, restart)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
	assert.True(t, f.columnExists(t, "hand_note"), "the column added by hand is kept")
	assert.True(t, f.indexOnLabelExists(t), "the index added by hand is kept")
	assert.False(t, f.slotExists(t, row.SlotName), "the restarted run dropped its slot at the swap")
}

// slotExists reports whether a replication slot of the name stands.
func (f liveFixture) slotExists(t *testing.T, slot string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, slot).Scan(&exists))
	return exists
}

// columnExists reports whether the workload table has the column.
func (f liveFixture) columnExists(t *testing.T, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)`,
		f.table.Schema, f.table.Table, column).Scan(&exists))
	return exists
}

// indexOnLabelExists reports whether the workload table carries an index
// keyed on label alone, under whatever name the swap gave it.
func (f liveFixture) indexOnLabelExists(t *testing.T) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_index i
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
			WHERE i.indrelid = $1::regclass AND i.indnatts = 1 AND a.attname = 'label')`,
		f.table.Qualified()).Scan(&exists))
	return exists
}
