package applier

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

func TestNewFlusherRejectsEmptyProof(t *testing.T) {
	_, err := NewFlusher(preflight.CopySwapTarget{}, nil, nil, Options{})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.EqualError(t, err, "invariant violation (ST-6): copy-and-swap target proof is empty")
}

func TestFlusherOptionsDefaults(t *testing.T) {
	opts := Options{}.withDefaults()
	assert.Equal(t, dbconn.DefaultLockTimeout, opts.LockTimeout)
	assert.Equal(t, dbconn.DefaultStatementTimeout, opts.StatementTimeout)
	require.NoError(t, opts.validate())

	given := Options{LockTimeout: time.Second, StatementTimeout: time.Minute}.withDefaults()
	assert.Equal(t, time.Second, given.LockTimeout)
	assert.Equal(t, time.Minute, given.StatementTimeout)
}

// A timeout the server would read as disabled is refused rather than
// defaulted (LK-2).
func TestFlusherOptionsRefuseUnboundedValues(t *testing.T) {
	cases := map[string]Options{
		"lock timeout below a millisecond":      {LockTimeout: 500 * time.Microsecond},
		"statement timeout below a millisecond": {StatementTimeout: time.Nanosecond},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			err := opts.withDefaults().validate()
			require.ErrorIs(t, err, ErrInvalidOptions)
			assert.Contains(t, err.Error(), "below PostgreSQL's one-millisecond resolution")
		})
	}
}

// The flush recognises the server's refusal of a colliding write by SQLSTATE
// alone: a unique index's and an exclusion constraint's, and no other.
func TestConstraintCollisionMatchesBySQLSTATE(t *testing.T) {
	assert.True(t, constraintCollision(fmt.Errorf("insert key 1: %w", &pgconn.PgError{Code: sqlstateUniqueViolation})))
	assert.True(t, constraintCollision(fmt.Errorf("insert key 1: %w", &pgconn.PgError{Code: sqlstateExclusionViolation})))
	assert.False(t, constraintCollision(fmt.Errorf("insert key 1: %w", &pgconn.PgError{Code: sqlstateUndefinedTable})))
	assert.False(t, constraintCollision(assert.AnError))
}
