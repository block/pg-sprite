package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A run's tracker reports every phase and hands each the work source, so
// it is refused beside a phase's own tracker, which would compete with it
// for the one source a tracker reads; and a step is refused without a
// tracker or below 1, where the snapshot would read as no step started.
func TestNewRunnerRefusesAMiswiredRunTracker(t *testing.T) {
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	for name, opts := range map[string]schemachange.RunOptions{
		"beside the copier's tracker":   {Tracker: tracker, Step: 1, Copier: copier.Options{Tracker: tracker}},
		"beside the checksum's tracker": {Tracker: tracker, Step: 1, Checksum: checksum.Options{Tracker: tracker}},
		"beside the catch-up's tracker": {Tracker: tracker, Step: 1, Catchup: applier.CatchupOptions{Tracker: tracker}},
		"with step zero":                {Tracker: tracker},
		"a step with no tracker":        {Step: 1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schemachange.NewRunner(dbconn.Config{}, &checkpoint.Store{}, opts)
			require.ErrorIs(t, err, schemachange.ErrInvalidOptions)
		})
	}
}
