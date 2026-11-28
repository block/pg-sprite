package migrate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Desired-state admission runs the route's resume-aware preflight, as the
// imperative door does: a plan whose copy-and-swap statement resumes a
// stopped live run is asked only for the disk left to grow. The table is
//
//	CREATE TABLE ledger (
//	    id   bigint PRIMARY KEY,
//	    qty  integer NOT NULL,
//	    note text NOT NULL
//	);
//
// and the desired schema retypes qty to bigint, which routes to
// copy-and-swap. The first desired-state run stops when its catch-up phase
// is saved, holding the whole shadow and its slot's WAL. At a free-disk
// figure that covers only the rest — the fresh run's preflight refuses
// it — the same desired schema is admitted, and the run resumes the
// stopped one over the shadow it built and converges.
func TestRunDesiredCopySwapResumeIsAskedOnlyForTheDiskLeftToGrow(t *testing.T) {
	base := newLiveFixture(t, testutil.WorkloadShape{})
	schema := base.table.Schema
	// The ledger is the table under test; the live fixture's helpers read
	// it through the schema and table name alone.
	f := liveFixture{url: base.url, pool: base.pool, table: testutil.WorkloadTable{Schema: schema, Table: "ledger"}}
	ledger := pgx.Identifier{schema, "ledger"}.Sanitize()
	f.exec(t, `
		CREATE TABLE `+ledger+` (
		    id   bigint PRIMARY KEY,
		    qty  integer NOT NULL,
		    note text NOT NULL
		)`)
	f.exec(t, `
		INSERT INTO `+ledger+` (id, qty, note)
		SELECT g, g, repeat('x', 200)
		FROM generate_series(1, 20000) AS g`)
	// Vacuumed up front, so autovacuum adds no visibility or free-space
	// map to the source between the test's size reading and admission's.
	f.exec(t, `VACUUM `+ledger)
	req := migrate.DesiredRequest{
		Schema: schema,
		Desired: parseDesired(t, `CREATE TABLE ledger (
			id   bigint PRIMARY KEY,
			qty  bigint NOT NULL,
			note text NOT NULL
		)`),
	}

	stopped := f.options()
	stopped.CopySwap.OnCheckpoint = func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCatchingUp {
			return errStopAtCatchUp
		}
		return nil
	}
	res, err := migrate.RunDesired(t.Context(), f.pool, req, stopped)
	require.ErrorIs(t, err, errStopAtCatchUp)
	require.Equal(t, verdict.OutcomeFailed, res.Outcome, "%s: %s", res.Reason, res.Detail)
	row := f.inspectRow(t)
	require.Equal(t, checkpoint.PhaseCatchingUp, row.Phase)
	require.NotEmpty(t, row.SlotName)

	shadow := schemachange.ShadowName(schema, "ledger")
	shadowOID := f.relationOID(t, shadow)
	tableBytes := f.totalRelationSize(t, "ledger")
	shadowBytes := f.totalRelationSize(t, shadow)
	retained := f.slotRetained(t, row.SlotName)
	require.Greater(t, shadowBytes, resumeDiskMargin, "the shadow alone is more than the margin, so the resume needs its discount")
	require.Less(t, retained, resumedRunCeiling, "the raised ceiling is above the WAL the slot retained")

	// What the volume has free: the rest of the shadow's headroom and the
	// rest of the raised ceiling, and no more.
	free := 2*tableBytes - shadowBytes + resumedRunCeiling - retained + resumeDiskMargin

	_, err = preflight.CheckCopySwap(t.Context(), f.pool, schema, "ledger", preflight.CopySwapEnvironment{
		LogicalDecoding: true, FreeDiskBytes: free, SlotLagCeilingBytes: resumedRunCeiling,
	})
	envErr, ok := errors.AsType[*preflight.CopySwapEnvironmentError](err)
	require.True(t, ok, "a fresh run at the figure is a disk refusal: %v", err)
	assert.Equal(t, preflight.CopySwapCauseDiskHeadroom, envErr.Cause)

	resume := f.options()
	resume.CopySwap.FreeDiskBytes = free
	resume.CopySwap.SlotLagCeilingBytes = resumedRunCeiling
	res, err = migrate.RunDesired(t.Context(), f.pool, req, resume)
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s: %s", res.Reason, res.Detail)
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, res.Verdicts[0].Outcome)
	assert.Equal(t, shadowOID, f.relationOID(t, "ledger"), "the stopped run's shadow is the live table: the run resumed it")
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)

	var qtyType string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT format_type(atttypid, atttypmod)
		FROM pg_attribute
		WHERE attrelid = $1::regclass
		  AND attname = 'qty'`, ledger).Scan(&qtyType))
	assert.Equal(t, "bigint", qtyType)
}

// relationOID is the OID of the relation name in the fixture's schema.
func (f liveFixture) relationOID(t *testing.T, name string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`,
		pgx.Identifier{f.table.Schema, name}.Sanitize()).Scan(&oid))
	return oid
}
