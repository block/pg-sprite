package schemachange_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A run has nowhere to write its phases without a store, so a nil one is
// refused at construction rather than at the first save.
func TestNewRunnerRefusesANilStore(t *testing.T) {
	_, err := schemachange.NewRunner(dbconn.Config{}, nil, schemachange.RunOptions{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
}

// The options every phase shares are validated at construction, so a run
// cannot build a shadow and then stop at its first save or its swap over
// a value it could have refused up front. The live pass reads the run's
// own catch-up, so a confirmed source handed in is refused too.
func TestNewRunnerRefusesOptionsARunCouldNotHonour(t *testing.T) {
	store := &checkpoint.Store{}
	for name, opts := range map[string]schemachange.RunOptions{
		"negative checkpoint interval":            {CheckpointInterval: -time.Second},
		"shadow lock timeout below a millisecond": {Shadow: schemachange.Options{LockTimeout: time.Microsecond}},
		"negative cutover lock attempts":          {Cutover: schemachange.CutoverOptions{LockAttempts: -1}},
		"a confirmed source for the live pass":    {Live: checksum.LiveOptions{Confirmed: (*applier.Catchup)(nil)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schemachange.NewRunner(dbconn.Config{}, store, opts)
			require.Error(t, err)
			assert.ErrorIs(t, err, schemachange.ErrInvalidOptions)
		})
	}
}

// Zero options are the defaults of every phase, so a runner over them is
// constructible.
func TestNewRunnerAcceptsZeroOptions(t *testing.T) {
	r, err := schemachange.NewRunner(dbconn.Config{}, &checkpoint.Store{}, schemachange.RunOptions{})
	require.NoError(t, err)
	assert.NotNil(t, r)
}
