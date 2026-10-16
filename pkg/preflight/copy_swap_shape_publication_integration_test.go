package preflight_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/preflight"
)

// A publication of another table that a second session drops while the
// re-check's transaction holds an older snapshot does not fail the
// re-check: the publications are read from the catalogs that record
// membership, which the snapshot answers as it was, not through
// pg_publication_tables, whose function looks every publication the
// snapshot lists up again by name and reports the dropped one as missing.
// A concurrent run dropping its own publication after its swap, while this
// run re-checks its shape under the swap lock, is the case.
func TestRecheckCopySwapShapeSurvivesAnotherTablesPublicationDroppedMeanwhile(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.other (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE PUBLICATION pub_other FOR TABLE %s.other`)
	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, "orders", preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)

	// The snapshot is taken with pub_other in it; the drop lands after.
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
	var listed bool
	require.NoError(t, tx.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_publication WHERE pubname = 'pub_other')`).Scan(&listed))
	require.True(t, listed, "the snapshot lists the publication about to be dropped")
	_, err = f.pool.Exec(t.Context(), `DROP PUBLICATION pub_other`)
	require.NoError(t, err)

	err = preflight.RecheckCopySwapShape(t.Context(), tx, target)

	assert.NoError(t, err, "a publication of another table, dropped since the snapshot, is not this table's concern")
}

// The re-check still refuses the table's own membership in a publication
// the snapshot shows, so reading the catalogs directly loses no refusal:
// a FOR TABLE publication of the table and a FOR ALL TABLES publication
// each refuse it. The fixture is its own database: a FOR ALL TABLES
// publication publishes every table in the database, so on the shared one
// it would refuse every other test's table.
func TestRecheckCopySwapShapeRefusesAPublicationOfTheTable(t *testing.T) {
	f := newCopySwapShapeFixtureInOwnDatabase(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY
		)`)
	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, "orders", preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)

	recheck := func(t *testing.T) error {
		t.Helper()
		tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
		return preflight.RecheckCopySwapShape(t.Context(), tx, target)
	}

	f.exec(t, `CREATE PUBLICATION pub_orders FOR TABLE %s.orders`)
	requireCopySwapCause(t, recheck(t), preflight.CopySwapCausePublicationMember)
	_, err = f.pool.Exec(t.Context(), `DROP PUBLICATION pub_orders`)
	require.NoError(t, err)
	require.NoError(t, recheck(t), "with the publication gone the table is admitted again")

	_, err = f.pool.Exec(t.Context(), `CREATE PUBLICATION pub_all FOR ALL TABLES`)
	require.NoError(t, err)
	requireCopySwapCause(t, recheck(t), preflight.CopySwapCausePublicationMember)
}
