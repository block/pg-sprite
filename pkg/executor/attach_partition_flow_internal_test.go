package executor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The online attach's admission runs before any connection is used, so
// these tests pass no pool: a refusal that reached the database would
// dereference it.

func attachAdmissionRefusal(t *testing.T, sql string) error {
	t.Helper()
	_, err := AttachPartitionOnline(t.Context(), dbconn.Config{}, nil, preflight.PreflightedTable{}, sql, attachFlowOptions)
	require.Error(t, err)
	return err
}

// A statement other than an ATTACH PARTITION is refused as a shape the
// online attach does not take.
func TestAttachPartitionOnlineRefusesAStatementThatIsNotAnAttach(t *testing.T) {
	err := attachAdmissionRefusal(t, `ALTER TABLE app.orders DETACH PARTITION app.orders_2027`)
	assert.ErrorIs(t, err, ErrAttachPartitionUnsupportedShape)
	assert.Equal(t, CodeAttachPartitionUnsupportedShape, OutcomeCode(err))
}

// The parent must be schema-qualified: the run's search_path is derived
// from the preflighted parent's schema, and an unqualified parent could
// resolve elsewhere.
func TestAttachPartitionOnlineRefusesAnUnqualifiedParent(t *testing.T) {
	err := attachAdmissionRefusal(t, `ALTER TABLE orders
		ATTACH PARTITION orders_2027
		FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
	assert.ErrorIs(t, err, ErrAttachPartitionUnsupportedShape)
}

// A statement on another table than the preflighted one breaks ST-7 and is
// refused fail-closed. The preflighted table here is the zero value, which
// names no table.
func TestAttachPartitionOnlineRefusesAStatementOnAnotherTableThanThePreflight(t *testing.T) {
	err := attachAdmissionRefusal(t, `ALTER TABLE app.orders
		ATTACH PARTITION app.orders_2027
		FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
	assert.ErrorIs(t, err, ErrInvariantViolation)
}

// Every budget is bounded: a holder wait below zero or past the server's
// int32-millisecond ceiling is refused before admission.
func TestAttachPartitionOptionsRefuseAnUnboundedHolderWait(t *testing.T) {
	require.NoError(t, attachFlowOptions.validate())
	for _, wait := range []time.Duration{-time.Second, maxOverallBudget + time.Millisecond} {
		o := attachFlowOptions
		o.HolderWait = wait
		assert.Error(t, o.validate(), "holder wait %s", wait)
	}
	o := attachFlowOptions
	o.Attach = Budget{}
	assert.Error(t, o.validate(), "an unset attach budget")
}
