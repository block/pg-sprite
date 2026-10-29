package checksum

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
)

func TestNewVerifierRejectsEmptyProof(t *testing.T) {
	_, err := NewVerifier(preflight.CopySwapTarget{}, nil, nil, Options{})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.EqualError(t, err, "invariant violation (ST-6): copy-and-swap target proof is empty")
}

func TestVerifierOptionsDefaults(t *testing.T) {
	opts := Options{}.withDefaults()
	assert.Equal(t, dbconn.DefaultLockTimeout, opts.LockTimeout)
	assert.Equal(t, dbconn.DefaultStatementTimeout, opts.StatementTimeout)
	assert.Equal(t, progress.WallClock{}, opts.Clock)
	assert.Equal(t, DefaultRecopyAttempts, opts.RecopyAttempts)
	require.NoError(t, opts.validate())

	given := Options{LockTimeout: time.Second, StatementTimeout: time.Minute, RecopyAttempts: 1}.withDefaults()
	assert.Equal(t, time.Second, given.LockTimeout)
	assert.Equal(t, time.Minute, given.StatementTimeout)
	assert.Equal(t, 1, given.RecopyAttempts)
}

// A timeout the server would read as disabled is refused rather than
// defaulted (LK-2), and so is a recopy attempt count that would recopy
// nothing.
func TestVerifierOptionsRefuseUnboundedValues(t *testing.T) {
	cases := map[string]Options{
		"lock timeout below a millisecond":      {LockTimeout: 500 * time.Microsecond},
		"statement timeout below a millisecond": {StatementTimeout: time.Nanosecond},
		"negative recopy attempts":              {RecopyAttempts: -1},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, opts.withDefaults().validate(), ErrInvalidOptions)
		})
	}
}

// A report is clean only when no chunk differed; how much it read does not
// enter into it.
func TestReportClean(t *testing.T) {
	chunk, err := copier.NewChunk(1, 10)
	require.NoError(t, err)
	assert.True(t, Report{Through: copier.NewWatermark(10), Chunks: 1, Rows: 10}.Clean())
	assert.False(t, Report{Through: copier.NewWatermark(10), Chunks: 1, Rows: 10, Mismatches: []Mismatch{{
		Chunk:  chunk,
		Source: Digest{Rows: 10, Hash: "a"},
		Shadow: Digest{Rows: 10, Hash: "b"},
	}}}.Clean())
}
