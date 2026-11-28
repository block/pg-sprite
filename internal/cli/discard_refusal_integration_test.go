package cli

import (
	"context"
	"fmt"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here drive the discard command's refusals and stops: which
// outcome and exit code a discard that does not drop everything reports,
// and what it leaves standing. They use the fixture of
// discard_integration_test.go.

// discardSlotName is the replication slot and publication name a run on
// the fixture table derives, read from the shape the discard proves.
func (f discardFixture) discardSlotName(t *testing.T) string {
	t.Helper()
	shape, _, err := proveDiscardShape(t.Context(), f.pool, f.schema, "orders")
	require.NoError(t, err)
	return shape.DecodingName()
}

// holdDiscardSlot starts streaming from the slot on a replication
// connection of its own, which makes the slot active until the connection
// closes at cleanup. It first waits for the slot's previous holder, the
// stopped run's walsender, to let go.
func holdDiscardSlot(t *testing.T, url string, pool *pgxpool.Pool, slot string) {
	t.Helper()
	const previousHolderGone = 10 * time.Second
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, found, err := decode.InspectSlot(t.Context(), pool, slot)
		if !assert.NoError(collect, err) || !assert.True(collect, found) {
			return
		}
		assert.False(collect, status.Active, "the stopped run's walsender has let go")
	}, previousHolderGone, 50*time.Millisecond)
	holder, err := dbconn.ConnectReplication(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, holder.Close(context.WithoutCancel(t.Context()))) })
	err = pglogrepl.StartReplication(t.Context(), holder, slot, 0, pglogrepl.StartReplicationOptions{
		Mode:       pglogrepl.LogicalReplication,
		PluginArgs: []string{"proto_version '1'", "publication_names '" + slot + "'"},
	})
	require.NoError(t, err)
	const slotHeld = 10 * time.Second
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, found, err := decode.InspectSlot(t.Context(), pool, slot)
		if !assert.NoError(collect, err) || !assert.True(collect, found) {
			return
		}
		assert.True(collect, status.Active, "the streaming walsender holds the slot")
	}, slotHeld, 50*time.Millisecond)
}

// deleteRows deletes every checkpoint row, as an operator who removed a
// run's row by hand leaves its leftovers unrecorded.
func (f discardFixture) deleteRows(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), `DELETE FROM pgsprite.pgsprite_checkpoint`)
	require.NoError(t, err)
}

// With no checkpoint row, the discard drops the shadow first and then
// meets the slot a walsender holds, which it drops only as the reaper
// would, so it leaves it. The shadow is gone, so the report is stopped,
// not refused: exit 1 with cause slot-active, naming the shadow. The slot
// and its publication stand.
func TestDiscardOfAHeldUnrecordedSlotStopsAfterTheShadow(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s"))
	stopped := f.stopRetypeAt(t, checkpoint.PhaseCatchingUp, false)
	f.deleteRows(t)
	holdDiscardSlot(t, f.url, f.pool, stopped.SlotName)

	report, err := runDiscard(t, f.url, f.table())
	require.Error(t, err)
	assert.NotErrorIs(t, err, verdict.ErrRefused, "a discard that dropped something exits 1, not 2")
	assert.ErrorIs(t, err, decode.ErrSlotActive)
	assert.Equal(t, discardOutcomeStopped, report.Outcome)
	assert.Equal(t, discardCauseSlotActive, report.Cause)
	assert.Equal(t, stopped.ShadowTable, report.Shadow)
	assert.Empty(t, report.Slot)
	assert.Empty(t, report.Publication)
	assert.False(t, f.relationPresent(t, stopped.ShadowTable))
	assert.True(t, slotPresent(t, f.pool, stopped.SlotName), "a held slot is its holder's")
	assert.True(t, publicationPresent(t, f.pool, stopped.PublicationName))
}

// A slot of the table's derived name with a publication of the same name
// that publishes every table, not the single-table publication the engine
// makes: with no row, the discard drops the slot, then refuses the
// publication. Nothing the call names is dropped, but the slot is gone, so
// the report is stopped with exit 1 and names the publication left.
func TestDiscardNamesAPublicationItLeavesAfterTheSlot(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgresWithSettings(t, "wal_level=logical"))
	name := f.discardSlotName(t)
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR ALL TABLES`, pgx.Identifier{name}.Sanitize()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, pgx.Identifier{name}.Sanitize()))
		assert.NoError(t, err)
	})
	_, err = f.pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
	require.NoError(t, err)
	t.Cleanup(func() { dropSlotIfPresent(t, f.pool, name) })

	report, err := runDiscard(t, f.url, f.table())
	require.Error(t, err)
	assert.ErrorIs(t, err, decode.ErrSlotDroppedPublicationLeft)
	assert.NotErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, discardOutcomeStopped, report.Outcome)
	assert.Equal(t, name, report.PublicationLeft)
	assert.Empty(t, report.Slot)
	assert.Empty(t, report.Publication)
	assert.False(t, slotPresent(t, f.pool, name), "the slot is gone")
	assert.True(t, publicationPresent(t, f.pool, name), "a publication not of the engine's shape is left")
}

// A completed change leaves its row done: there is no run to discard, so
// the discard is refused as resume-row-terminal with exit 2, and the row
// stands.
func TestDiscardRefusesACompletedChange(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgres(t))
	require.NoError(t, f.runRetype(t, true, nil))
	row, found := f.row(t)
	require.True(t, found)
	require.Equal(t, checkpoint.PhaseDone, row.Phase)

	report, err := runDiscard(t, f.url, f.table())
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, discardReport{
		FormatVersion: discardFormatVersion,
		Table:         f.table(),
		Outcome:       discardOutcomeRefused,
		Cause:         string(schemachange.CauseResumeTerminal),
		Detail:        report.Detail,
	}, report)
	row, found = f.row(t)
	require.True(t, found, "the done row stands")
	assert.Equal(t, checkpoint.PhaseDone, row.Phase)
}

// S.x.orders names two tables: "x.orders" in schema S, and "orders" in
// schema "S.x". The discard does not guess between them: it fails with
// exit 1, prints no report, and drops nothing.
func TestDiscardOfANameThatSplitsTwoWaysFails(t *testing.T) {
	f := newDiscardFixture(t, testutil.StartPostgres(t))
	dotted := pgx.Identifier{f.schema + ".x"}.Sanitize()
	_, err := f.pool.Exec(t.Context(), `CREATE SCHEMA `+dotted)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `DROP SCHEMA IF EXISTS `+dotted+` CASCADE`)
		assert.NoError(t, err)
	})
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id int PRIMARY KEY
		)`, dotted))
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s (
			id int PRIMARY KEY
		)`, pgx.Identifier{f.schema, "x.orders"}.Sanitize()))
	require.NoError(t, err)

	cmd := &DiscardCmd{DBFlags: DBFlags{URL: f.url}, Table: f.schema + ".x.orders"}
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	assert.ErrorIs(t, err, errDiscardTableAmbiguous)
	assert.NotErrorIs(t, err, verdict.ErrRefused)
	assert.Empty(t, out.String())
}

// discardRolePassword is the throwaway login role's password.
const discardRolePassword = "discard-test-only"

// newRoleDiscardFixture is the discard fixture on a database of its own,
// with orders owned by a login role created with options that may create
// in the schema and the database, so the role's runs and discards create
// and keep the checkpoint table as their own. The fixture connects as the
// role; admin is the superuser pool.
func newRoleDiscardFixture(t *testing.T, serverURL, options string) (f discardFixture, role string, admin *pgxpool.Pool) {
	t.Helper()
	dbURL := testutil.NewDatabase(t, serverURL)
	super := newDiscardFixture(t, dbURL)
	role = testutil.NewRole(t, super.pool, options+" PASSWORD '"+discardRolePassword+"'")
	t.Cleanup(func() {
		// The role owns the table, the checkpoint table, and a live run's
		// publication; hand them back so the role can be dropped.
		ctx := context.WithoutCancel(t.Context())
		_, err := super.pool.Exec(ctx, `REASSIGN OWNED BY `+pgx.Identifier{role}.Sanitize()+` TO CURRENT_USER`)
		assert.NoError(t, err)
		_, err = super.pool.Exec(ctx, `DROP OWNED BY `+pgx.Identifier{role}.Sanitize())
		assert.NoError(t, err)
	})
	var database string
	require.NoError(t, super.pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database))
	for _, grant := range []string{
		fmt.Sprintf(`GRANT CREATE ON DATABASE %s TO %s`, pgx.Identifier{database}.Sanitize(), pgx.Identifier{role}.Sanitize()),
		fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA %s TO %s`, pgx.Identifier{super.schema}.Sanitize(), pgx.Identifier{role}.Sanitize()),
		fmt.Sprintf(`ALTER TABLE %s OWNER TO %s`, pgx.Identifier{super.schema, "orders"}.Sanitize(), pgx.Identifier{role}.Sanitize()),
	} {
		_, err := super.pool.Exec(t.Context(), grant)
		require.NoError(t, err)
	}
	u, err := neturl.Parse(dbURL)
	require.NoError(t, err)
	u.User = neturl.UserPassword(role, discardRolePassword)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return discardFixture{url: u.String(), pool: pool, schema: super.schema}, role, super.pool
}

// A role without replication access can still discard an offline run,
// which holds no slot: the command proves the table again without
// replication access when the role lacks it. The role ran the change
// itself, offline, and discards it with exit 0.
func TestDiscardOfAnOfflineRunNeedsNoReplicationAccess(t *testing.T) {
	f, _, _ := newRoleDiscardFixture(t, testutil.StartPostgres(t), "LOGIN NOREPLICATION")
	stopped := f.stopRetypeAt(t, checkpoint.PhaseVerifying, true)

	report, err := runDiscard(t, f.url, f.table())
	require.NoError(t, err)
	assert.Equal(t, discardReport{
		FormatVersion: discardFormatVersion,
		Table:         f.table(),
		Outcome:       discardOutcomeDiscarded,
		RowPhase:      "verifying",
		Shadow:        stopped.ShadowTable,
		WriteFence:    true,
	}, report)
	_, found := f.row(t)
	assert.False(t, found, "the checkpoint row is deleted")
	f.assertSourceUntouched(t, 1000)
}

// A live run's slot is dropped over a replication connection, so a role
// that has lost replication access since the run stopped is refused with
// insufficient-privileges and exit 2 before anything is dropped: the
// shadow, the slot, the publication, and the row stand.
func TestDiscardRefusesALiveRunForARoleWithoutReplicationAccess(t *testing.T) {
	f, role, admin := newRoleDiscardFixture(t, testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s"), "LOGIN REPLICATION")
	stopped := f.stopRetypeAt(t, checkpoint.PhaseCatchingUp, false)
	_, err := admin.Exec(t.Context(), `ALTER ROLE `+pgx.Identifier{role}.Sanitize()+` NOREPLICATION`)
	require.NoError(t, err)

	report, err := runDiscard(t, f.url, f.table())
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, discardOutcomeRefused, report.Outcome)
	assert.Equal(t, string(verdict.ReasonInsufficientPrivileges), report.Cause)
	assert.Empty(t, report.Shadow)
	assert.Empty(t, report.Slot)
	assert.True(t, f.relationPresent(t, stopped.ShadowTable), "the shadow stands")
	assert.True(t, slotPresent(t, admin, stopped.SlotName), "the slot stands")
	assert.True(t, publicationPresent(t, admin, stopped.PublicationName))
	row, found := f.row(t)
	require.True(t, found, "the row stands")
	assert.Equal(t, checkpoint.PhaseCatchingUp, row.Phase)
}
