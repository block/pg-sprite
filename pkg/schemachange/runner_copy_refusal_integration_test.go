package schemachange_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests below drive the Runner into the copier's refusals and the
// copier's pass-through errors, and read each from Run's error the way an
// importer would: a copier refusal is re-minted as a shadow cause and
// answers in both vocabularies; a row the shadow rejects is the server's
// error and carries no cause.

// A shadow dropped and recreated under its own name after the row is
// written is not the relation the proof names: the copier's first chunk
// refuses it as copy-relation-replaced, and Run surfaces that as
// cutover-relation-replaced (ST-6) under ErrInvariantViolation, with the
// copier's own cause still readable. The row stays in copying for the
// operator to abandon.
func TestRunnerSurfacesAReplacedShadowAsARelationRefusal(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	// Replace the shadow once, on the first copying save, before the copier
	// has opened a connection: a crash-and-rebuild by another process.
	var once sync.Once
	replace := func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying {
			return nil
		}
		var err error
		once.Do(func() {
			ddl := `DROP TABLE %s.` + shadow + `; CREATE TABLE %s.` + shadow + ` (id bigint PRIMARY KEY, qty integer NOT NULL, sku text)`
			_, err = f.pool.Exec(ctx, strings.ReplaceAll(ddl, "%s", f.schema))
		})
		return err
	}

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{AfterCheckpoint: replace})
	require.Error(t, err)
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
	assert.Equal(t, copier.CauseRelationReplaced, copier.RefusalCauseOf(err), "the copier's cause is still readable through the Runner's")
	assert.Contains(t, err.Error(), "copy of "+f.schema+".orders refused")
	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t, "orders").Phase, "the row stands for the operator to abandon")
}

// A CHECK the gated statement adds that the source's rows violate is the
// shadow rejecting a chunk INSERT: the server's error, SQLSTATE 23514,
// reachable with errors.As and carrying no refusal cause in either
// vocabulary. The Runner passes it through unchanged, wrapped in an
// AbandonedError: a data fault meets the same rows on every resume, so the
// run is discarded and no row stands.
func TestRunnerPassesARowTheShadowRejectsThroughUnchanged(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders ADD CONSTRAINT qty_small CHECK (qty < 100)`, schemachange.RunOptions{})
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "the shadow's rejection is the server's error: %v", err)
	assert.Equal(t, "23514", pgErr.Code)
	assert.NotErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Empty(t, schemachange.RefusalCauseOf(err), "a row the shadow rejects is not a refusal")
	assert.Empty(t, copier.RefusalCauseOf(err))
	var abandoned *schemachange.AbandonedError
	assert.ErrorAs(t, err, &abandoned)
	_, err = f.store.Inspect(t.Context(), f.schema, "orders")
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "the abandoned run leaves no row")
}

// A copy stopped by a catalog change under a name its USING expression
// spells — here an integer * integer planted in the source schema once the
// row is in copying — is refused as the same environmental drift when it is
// rerun while the change is still in place: the shadow under the row's name
// is the one the build made, and the proof the resume re-derives differs
// from the row's in its conversion fingerprint alone. Undoing the change
// makes the fingerprints agree again, and the same rerun completes.
func TestRunnerResumeOverUnresolvedConversionDriftIsRefusedAsDrift(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `
		ALTER TABLE %s.orders
			DROP COLUMN note,
			ALTER COLUMN qty TYPE bigint USING (qty * 1)::bigint`
	var once sync.Once
	plant := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying {
			return nil
		}
		once.Do(func() {
			f.exec(t, `
				CREATE FUNCTION %s.times_zero(integer, integer) RETURNS integer
					LANGUAGE sql IMMUTABLE
					AS 'SELECT 0'`)
			f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = integer, RIGHTARG = integer, FUNCTION = %s.times_zero)`)
		})
		return nil
	}
	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: plant})
	require.Equal(t, schemachange.CauseCopyConversionDrift, schemachange.RefusalCauseOf(err))

	_, err = f.run(t, "orders", alter, schemachange.RunOptions{})
	require.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseCopyConversionDrift, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t, "orders").Phase, "the refused resume wrote nothing")

	f.exec(t, `DROP OPERATOR %s.* (integer, integer)`)
	f.exec(t, `DROP FUNCTION %s.times_zero(integer, integer)`)
	result, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
}
