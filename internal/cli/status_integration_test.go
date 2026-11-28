package cli

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// The tests here stop a copy-and-swap run of
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// the way a kill or a Ctrl-C leaves it, and check that status and reap
// name what the stopped run holds and the discard that lets it go, and
// that running the statement again says it resumes the run. Each gets a
// database of its own: status lists every unfinished row and pg-sprite
// session in the connected database.

// newStoppedLiveRun creates orders on a server that decodes WAL and stops
// a live run of the retype at its first save in copying, with its shadow,
// its slot, and its row standing. Rows written to the source afterwards
// grow the WAL the slot retains, as the cluster's writes do in production.
func newStoppedLiveRun(t *testing.T) (discardFixture, checkpoint.Checkpoint) {
	t.Helper()
	server := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	f := newDiscardFixture(t, testutil.NewDatabase(t, server))
	stopped := f.stopRetypeAt(t, checkpoint.PhaseCopying, false)
	require.NotEmpty(t, stopped.SlotName, "a live run names its slot")
	f.exec(t, `
		INSERT INTO %s.orders (id, qty)
		SELECT g, g * 10
		FROM generate_series(1001, 2000) g`)
	return f, stopped
}

// A live run stopped mid-copy is listed by status: its table, statement,
// phase, and last save, its inactive slot with the WAL it retains, no
// write fence, and the discard command. The text names the same and ends
// with both ways past the run. While a session holds the table's lock, as
// a run in progress does, the same row is listed as running, with that
// session's pid.
func TestStatusListsAStoppedLiveRunWithItsSlotAndTheDiscard(t *testing.T) {
	f, stopped := newStoppedLiveRun(t)

	report := runStatusJSON(t, f.url)

	assert.Equal(t, statusFormatVersion, report.FormatVersion)
	assert.Equal(t, checkpointTablePresent, report.CheckpointTable)
	require.Len(t, report.StoppedRuns, 1)
	run := report.StoppedRuns[0]
	assert.Equal(t, f.table(), run.Table)
	assert.Equal(t, stopped.Statement, run.Statement)
	assert.Equal(t, "copying", run.Phase)
	assert.True(t, stopped.UpdatedAt.Equal(run.UpdatedAt), "last saved %s, reported %s", stopped.UpdatedAt, run.UpdatedAt)
	assert.False(t, run.Running)
	assert.Zero(t, run.RunningPID)
	assert.False(t, run.WriteFence, "a live run fences nothing")
	require.NotNil(t, run.Slot)
	assert.Equal(t, stopped.SlotName, run.Slot.Name)
	assert.True(t, run.Slot.Exists)
	assert.False(t, run.Slot.Active, "nothing streams from a stopped run's slot")
	assert.Equal(t, string(decode.WALStatusReserved), run.Slot.WALStatus)
	require.NotNil(t, run.Slot.RetainedBytes)
	assert.Positive(t, *run.Slot.RetainedBytes, "the slot keeps the WAL written since it was made")

	cmd := &StatusCmd{DBFlags: DBFlags{URL: f.url}}
	var text strings.Builder
	require.NoError(t, cmd.run(t.Context(), &text))
	assert.Contains(t, text.String(), f.table()+": stopped in phase copying")
	assert.Contains(t, text.String(), "replication slot "+stopped.SlotName+": inactive, wal_status reserved, retaining ")
	assert.Contains(t, text.String(), "pg-sprite discard "+f.table()+"\n")

	holder, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: f.url}, f.schema, "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, holder.Release(context.WithoutCancel(t.Context()))) })
	held := runStatusJSON(t, f.url)
	require.Len(t, held.StoppedRuns, 1)
	assert.True(t, held.StoppedRuns[0].Running)
	assert.Equal(t, holder.BackendPID(), held.StoppedRuns[0].RunningPID)
}

// reap --dry-run keeps the stopped run's slot as live and names the table
// whose run owns it, in the JSON and beside the slot in the text with the
// discard command that lets it go.
func TestReapDryRunNamesTheDiscardForAStoppedRunsSlot(t *testing.T) {
	f, stopped := newStoppedLiveRun(t)

	report := runReap(t, &ReapCmd{DBFlags: DBFlags{URL: f.url}, DryRun: true})

	assert.Equal(t, []string{stopped.SlotName}, report.Live)
	assert.Equal(t, []reapLiveRun{{Slot: stopped.SlotName, Table: f.table()}}, report.LiveRuns)
	assert.Empty(t, report.WouldDrop)

	var text strings.Builder
	require.NoError(t, (&ReapCmd{DBFlags: DBFlags{URL: f.url}, DryRun: true}).run(t.Context(), &text))
	assert.Contains(t, text.String(), "  "+stopped.SlotName+"  run on "+f.table()+
		"; resume it with the same migrate command, or let it go: pg-sprite discard "+f.table()+"\n")
}

// Running the stopped retype again resumes it, and the run says so on
// stderr without --debug, naming the table and the phase it resumes from.
// The record goes through the always-on operator logger, which a run
// without --debug still writes.
func TestMigrateResumeOfAStoppedRunSaysSoWithoutDebug(t *testing.T) {
	f, _ := newStoppedLiveRun(t)

	cmd := f.migrateCmd(false)
	cmd.JSON = true
	var diag syncWriter
	cmd.diagOut = &diag
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out), out.String())

	assert.False(t, cmd.Debug)
	assert.Contains(t, diag.String(), `level=INFO msg="resuming a stopped copy-and-swap run"`)
	assert.Contains(t, diag.String(), fmt.Sprintf("schema=%s table=orders phase=copying", f.schema))
	assert.Equal(t, "bigint", f.qtyType(t), "the resumed run finished the change")
}

// An offline run stopped while it verifies keeps no slot and leaves the
// source fenced, so status reports a null slot and the fence, and its
// resume step carries --block-writes, as the resume needs it.
func TestStatusShowsTheWriteFenceOfAStoppedOfflineRun(t *testing.T) {
	f := newDiscardFixture(t, testutil.NewDatabase(t, testutil.StartPostgres(t)))
	f.stopRetypeAt(t, checkpoint.PhaseVerifying, true)

	report := runStatusJSON(t, f.url)

	require.Len(t, report.StoppedRuns, 1)
	run := report.StoppedRuns[0]
	assert.Equal(t, "verifying", run.Phase)
	assert.Nil(t, run.Slot, "an offline run decodes no WAL")
	assert.True(t, run.WriteFence)

	cmd := &StatusCmd{DBFlags: DBFlags{URL: f.url}}
	var text strings.Builder
	require.NoError(t, cmd.run(t.Context(), &text))
	assert.Contains(t, text.String(), "write fence: installed")
	assert.Contains(t, text.String(), "--block-writes "+f.table()+" to resume, or abandon the change with: pg-sprite discard "+f.table()+"\n")
}

// A role that may not read the checkpoint table cannot know whether a run
// is stopped, so status says the runs are unknown rather than that there
// are none, and still lists the sessions.
func TestStatusReportsACheckpointTableTheRoleMayNotRead(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	require.NoError(t, store.Ensure(t.Context()))

	// Roles are cluster-global, so a unique name plus a cleanup drop keeps
	// a shared server reusable.
	role := fmt.Sprintf("status_reader_%d", os.Getpid())
	_, err = pool.Exec(t.Context(), "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD 'status-test-only'")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
		assert.NoError(t, err)
	})
	u, err := neturl.Parse(url)
	require.NoError(t, err)
	u.User = neturl.UserPassword(role, "status-test-only")

	report := runStatusJSON(t, u.String())

	assert.Equal(t, checkpointTablePermissionDenied, report.CheckpointTable)
	assert.Equal(t, []statusRun{}, report.StoppedRuns)
	cmd := &StatusCmd{DBFlags: DBFlags{URL: u.String()}}
	var text strings.Builder
	require.NoError(t, cmd.run(t.Context(), &text))
	assert.Contains(t, text.String(), "copy-and-swap runs: unknown: this role may not read pgsprite.pgsprite_checkpoint")
}
