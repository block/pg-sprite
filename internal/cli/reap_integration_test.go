package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// reapFixture is one database on a cluster that decodes WAL, holding the
// three kinds of pgsprite_ slot the reap command sorts without a holder:
// an orphan no checkpoint row names, a slot a copying row names, and a
// prefix-wearing name without the engine's shape.
type reapFixture struct {
	url     string
	pool    *pgxpool.Pool
	orphan  string
	live    string
	foreign string
}

// newReapFixture builds the fixture: the checkpoint table, a table and an
// engine-shaped slot with its publication for each of ledger (the orphan)
// and orders (the live slot, claimed by a row in phase copying), and the
// foreign slot pgsprite_backup. Every slot left after the test is dropped.
func newReapFixture(t *testing.T) reapFixture {
	t.Helper()
	url := testutil.NewDatabase(t, testutil.StartPostgresWithSettings(t, "wal_level=logical"))
	cfg := dbconn.Config{URL: url}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	require.NoError(t, store.Ensure(t.Context()))
	schema := testutil.NewSchema(t, pool)

	f := reapFixture{url: url, pool: pool, foreign: "pgsprite_backup"}
	f.orphan = reapSlotFor(t, cfg, pool, schema, "ledger")
	f.live = reapSlotFor(t, cfg, pool, schema, "orders")

	lock, err := dbconn.AcquireTableLock(t.Context(), cfg, schema, "orders")
	require.NoError(t, err)
	require.NoError(t, store.Save(t.Context(), lock, checkpoint.Checkpoint{
		Schema:            schema,
		Table:             "orders",
		ShadowTable:       "_pgsprite_orders_new",
		SlotName:          f.live,
		PublicationName:   f.live,
		LastAppliedLSN:    decode.LSN(1),
		SourceFingerprint: "src-orders",
		TargetFingerprint: "tgt-orders",
		Phase:             checkpoint.PhaseCopying,
	}))
	require.NoError(t, lock.Release(t.Context()))

	_, err = pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, f.foreign)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_drop_replication_slot($1)`, f.foreign)
		assert.NoError(t, err)
	})
	return f
}

// reapSlotFor creates the table, proves it for a copy-and-swap run that
// decodes WAL, and creates its slot and publication as such a run would,
// closing the slot's connection so nothing holds it. It returns the slot's
// name.
func reapSlotFor(t *testing.T, cfg dbconn.Config, pool *pgxpool.Pool, schema, table string) string {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s (
			id bigint PRIMARY KEY,
			note text
		)`, pgx.Identifier{schema, table}.Sanitize()))
	require.NoError(t, err)
	target, err := preflight.CheckCopySwap(t.Context(), pool, schema, table,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	slot, err := decode.CreateSlot(t.Context(), cfg, pool, target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), cfg, pool, slot.Name()))
	})
	return slot.Name()
}

// slotPresent reports whether the cluster has a slot of the name.
func slotPresent(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var present bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_replication_slots
			WHERE slot_name = $1)`, name).Scan(&present))
	return present
}

// publicationPresent reports whether the database has a publication of the
// name.
func publicationPresent(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var present bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_publication
			WHERE pubname = $1)`, name).Scan(&present))
	return present
}

// runReap runs the reap command with --json and decodes its report.
func runReap(t *testing.T, cmd *ReapCmd) reapReport {
	t.Helper()
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var report reapReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	return report
}

// --dry-run names the orphan as the one slot a run would drop and the live
// and foreign slots as kept, and drops nothing: every slot and the orphan's
// publication are still there after it.
func TestReapDryRunListsTheOrphanAndDropsNothing(t *testing.T) {
	f := newReapFixture(t)

	report := runReap(t, &ReapCmd{DBFlags: DBFlags{URL: f.url}, DryRun: true})

	assert.Equal(t, reapReport{
		DryRun:                true,
		CheckpointTableExists: true,
		Dropped:               []string{},
		WouldDrop:             []string{f.orphan},
		Live:                  []string{f.live},
		Active:                []string{},
		Foreign:               []string{f.foreign},
	}, report)
	assert.True(t, slotPresent(t, f.pool, f.orphan), "a dry run leaves the orphan's slot")
	assert.True(t, publicationPresent(t, f.pool, f.orphan), "a dry run leaves the orphan's publication")
	assert.True(t, slotPresent(t, f.pool, f.live))
	assert.True(t, slotPresent(t, f.pool, f.foreign))
}

// A real run drops the orphan with its publication and keeps the slot a
// copying row names and the foreign name; a second run finds nothing left
// to drop. The human-readable report renders without error too.
func TestReapDropsTheOrphanAndKeepsLiveAndForeignSlots(t *testing.T) {
	f := newReapFixture(t)

	report := runReap(t, &ReapCmd{DBFlags: DBFlags{URL: f.url}})

	assert.Equal(t, reapReport{
		CheckpointTableExists: true,
		Dropped:               []string{f.orphan},
		WouldDrop:             []string{},
		Live:                  []string{f.live},
		Active:                []string{},
		Foreign:               []string{f.foreign},
	}, report)
	assert.False(t, slotPresent(t, f.pool, f.orphan))
	assert.False(t, publicationPresent(t, f.pool, f.orphan), "the orphan's publication goes with its slot")
	assert.True(t, slotPresent(t, f.pool, f.live))
	assert.True(t, publicationPresent(t, f.pool, f.live))
	assert.True(t, slotPresent(t, f.pool, f.foreign))

	again := runReap(t, &ReapCmd{DBFlags: DBFlags{URL: f.url}})
	assert.Empty(t, again.Dropped)
	assert.Equal(t, []string{f.live}, again.Live)

	var text strings.Builder
	require.NoError(t, (&ReapCmd{DBFlags: DBFlags{URL: f.url}}).run(t.Context(), &text))
	assert.NotEmpty(t, text.String())
}

// The JSON report's lists are arrays even when empty, never null, so a
// consumer can iterate every key without a nil check.
func TestReapJSONListsAreArraysWhenEmpty(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgresWithSettings(t, "wal_level=logical"))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	require.NoError(t, store.Ensure(t.Context()))

	cmd := &ReapCmd{DBFlags: DBFlags{URL: url}, JSON: true}
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out.String()), &raw))
	for _, key := range []string{"dropped", "would_drop", "live", "active", "foreign"} {
		assert.JSONEq(t, `[]`, string(raw[key]), "%s is an empty array", key)
	}
	assert.JSONEq(t, `true`, string(raw["checkpoint_table_exists"]))
	assert.JSONEq(t, `false`, string(raw["dry_run"]))
	assert.JSONEq(t, `false`, string(raw["stopped"]))
}

// A drop that fails partway still reports what was dropped before it, marks
// the report stopped, and exits 1. The second orphan's publication publishes
// every table, which is not the single-table publication the engine makes,
// so its drop fails after the first orphan has gone. Both names are made by
// hand in the engine's shape so the pass reaches them in this order.
func TestReapReportsWhatWasDroppedBeforeAFailedDrop(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgresWithSettings(t, "wal_level=logical"))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	require.NoError(t, store.Ensure(t.Context()))
	schema := testutil.NewSchema(t, pool)
	ledger := pgx.Identifier{schema, "ledger"}.Sanitize()
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s (
			id bigint PRIMARY KEY
		)`, ledger))
	require.NoError(t, err)

	const first, second = "pgsprite_0000aaaa", "pgsprite_ffff0000"
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s`, first, ledger))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR ALL TABLES`, second))
	require.NoError(t, err)
	for _, name := range []string{first, second} {
		_, err = pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx := context.WithoutCancel(t.Context())
			_, err := pool.Exec(ctx, `
				SELECT pg_drop_replication_slot(slot_name)
				FROM pg_catalog.pg_replication_slots
				WHERE slot_name = $1`, name)
			assert.NoError(t, err)
		})
	}

	cmd := &ReapCmd{DBFlags: DBFlags{URL: url}, JSON: true}
	var out strings.Builder
	require.Error(t, cmd.run(t.Context(), &out))
	var report reapReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, reapReport{
		CheckpointTableExists: true,
		Stopped:               true,
		Dropped:               []string{first},
		WouldDrop:             []string{},
		Live:                  []string{},
		Active:                []string{},
		Foreign:               []string{},
	}, report)
	assert.False(t, slotPresent(t, pool, first))
	assert.False(t, publicationPresent(t, pool, first))
	assert.True(t, publicationPresent(t, pool, second), "a publication not of the engine's shape is left")

	// The failed drop took the second slot and left its publication, so
	// recreating both slots and the first publication sets the same failure
	// up again for the exit code through Main.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s`, first, ledger))
	require.NoError(t, err)
	for _, name := range []string{first, second} {
		_, err = pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, Main([]string{"reap", "--url", url, "--json"}, "test", io.Discard, io.Discard))
}

// A database where no schema change has run has no checkpoint table, so no
// slot is the engine's to reap: the command reports the table absent with
// empty lists and exits 0, in a dry run and a real run alike, and leaves an
// engine-shaped slot it cannot vouch for where it is.
func TestReapReportsAnAbsentCheckpointTableAndExitsZero(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgresWithSettings(t, "wal_level=logical"))
	cfg := dbconn.Config{URL: url}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	slot := reapSlotFor(t, cfg, pool, schema, "orders")

	for _, dryRun := range []bool{true, false} {
		report := runReap(t, &ReapCmd{DBFlags: DBFlags{URL: url}, DryRun: dryRun})
		assert.Equal(t, reapReport{
			DryRun:    dryRun,
			Dropped:   []string{},
			WouldDrop: []string{},
			Live:      []string{},
			Active:    []string{},
			Foreign:   []string{},
		}, report, "dry run %t", dryRun)

		args := []string{"reap", "--url", url, "--json"}
		if dryRun {
			args = append(args, "--dry-run")
		}
		assert.Equal(t, 0, Main(args, "test", io.Discard, io.Discard), "dry run %t", dryRun)
	}
	assert.True(t, slotPresent(t, pool, slot))
}
