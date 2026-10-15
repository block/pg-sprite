package copier

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
)

func TestNewCopierRejectsEmptyProof(t *testing.T) {
	_, err := NewCopier(preflight.CopySwapTarget{}, nil, nil, Watermark{}, Options{})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseProofEmpty, RefusalCauseOf(err))
	assert.EqualError(t, err, "invariant violation: ST-6 (copy-proof-empty): copy-and-swap target proof is empty")
}

// The ledger faults the copier refuses on each carry their own cause: a
// chunk landed that was never in flight and a clean stop with a chunk
// still in flight are the ledger and its workers disagreeing (LK-3); a
// clean stop short of the key space is the frontier broken (CO-4).
func TestCopierLedgerFaultsCarryTheirOwnCause(t *testing.T) {
	newCopier := func() *Copier {
		return &Copier{lock: &dbconn.TableLockSession{}, ledger: newLedger(Watermark{})}
	}
	chunk := mustChunk(t, math.MinInt64, 1000)

	err := newCopier().land(chunk, 10)
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseChunksInFlight, RefusalCauseOf(err), "a landed chunk the ledger never had in flight")

	c := newCopier()
	require.True(t, c.ledger.claim(chunk))
	err = c.finish(nil)
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseChunksInFlight, RefusalCauseOf(err), "a clean stop with a chunk still in flight")

	err = newCopier().finish(nil)
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseFrontierBroken, RefusalCauseOf(err), "a clean stop short of the key space")

	stopped := errors.New("stopped by the caller")
	assert.Same(t, stopped, newCopier().finish(stopped), "a stop with a reason is that reason, not a ledger fault")
}

func TestCopierOptionsDefaults(t *testing.T) {
	opts := Options{}.withDefaults()
	assert.Equal(t, DefaultWorkers, opts.Workers)
	assert.Equal(t, dbconn.DefaultLockTimeout, opts.LockTimeout)
	assert.Equal(t, dbconn.DefaultStatementTimeout, opts.StatementTimeout)
	assert.Equal(t, progress.WallClock{}, opts.Clock)
	require.NoError(t, opts.validate())

	given := Options{Workers: 2, LockTimeout: time.Second, StatementTimeout: time.Minute}.withDefaults()
	assert.Equal(t, 2, given.Workers)
	assert.Equal(t, time.Second, given.LockTimeout)
	assert.Equal(t, time.Minute, given.StatementTimeout)
}

// A timeout the server would read as disabled, or a worker count that
// copies nothing, is refused rather than defaulted (LK-2).
func TestCopierOptionsRefuseUnboundedValues(t *testing.T) {
	cases := map[string]Options{
		"negative workers":                      {Workers: -1},
		"lock timeout below a millisecond":      {LockTimeout: 500 * time.Microsecond},
		"statement timeout below a millisecond": {StatementTimeout: time.Nanosecond},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, opts.withDefaults().validate(), ErrInvalidOptions)
		})
	}
}
