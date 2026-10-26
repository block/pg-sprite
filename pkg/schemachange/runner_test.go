package schemachange_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
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
// own catch-up, so a confirmed source or a flush gate handed in is refused
// too.
func TestNewRunnerRefusesOptionsARunCouldNotHonour(t *testing.T) {
	store := &checkpoint.Store{}
	for name, opts := range map[string]schemachange.RunOptions{
		"negative checkpoint interval":            {CheckpointInterval: -time.Second},
		"shadow lock timeout below a millisecond": {Shadow: schemachange.Options{LockTimeout: time.Microsecond}},
		"negative cutover lock attempts":          {Cutover: schemachange.CutoverOptions{LockAttempts: -1}},
		"a confirmed source for the live pass":    {Live: checksum.LiveOptions{Confirmed: (*applier.Catchup)(nil)}},
		"a flush gate for the live pass":          {Live: checksum.LiveOptions{Flushes: (*applier.Catchup)(nil)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schemachange.NewRunner(dbconn.Config{}, store, opts)
			require.Error(t, err)
			assert.ErrorIs(t, err, schemachange.ErrInvalidOptions)
		})
	}
}

// The copier's options are refused at construction too, rather than when
// the copy starts, after the shadow is built and the source fenced or its
// slot made. The error carries the copier's own sentinel beside the
// Runner's, so a caller can match either.
func TestNewRunnerRefusesCopierOptionsTheCopyWould(t *testing.T) {
	store := &checkpoint.Store{}
	for name, tc := range map[string]struct {
		opts     copier.Options
		sentinel error
	}{
		"negative copier workers":  {copier.Options{Workers: -1}, copier.ErrInvalidOptions},
		"chunk floor over ceiling": {copier.Options{Chunker: copier.ChunkerOptions{MinRows: 50, MaxRows: 10}}, copier.ErrInvalidChunkerOptions},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schemachange.NewRunner(dbconn.Config{}, store, schemachange.RunOptions{Copier: tc.opts})
			require.Error(t, err)
			assert.ErrorIs(t, err, schemachange.ErrInvalidOptions)
			assert.ErrorIs(t, err, tc.sentinel)
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
