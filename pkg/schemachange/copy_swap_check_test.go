package schemachange

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/preflight"
)

// The preflight refuses, before it reads anything, a start that says
// neither resume nor restart: a caller that leaves it zero would otherwise
// be checked one way or the other without having chosen, and checking a
// restart as a resume asks it for less disk than it will use. A resume
// with no checkpoint store is refused too, since the measure reads the
// stopped run's row through it. Neither call touches the pool, which is
// nil here.
func TestCheckCopySwapRefusesAnUnchosenStartAndAResumeWithoutAStore(t *testing.T) {
	env := preflight.CopySwapEnvironment{FreeDiskBytes: 1 << 30}

	_, err := CheckCopySwap(t.Context(), nil, nil, "public", "orders", env, 0)
	require.ErrorIs(t, err, ErrInvalidOptions, "a zero start")

	_, err = CheckCopySwap(t.Context(), nil, nil, "public", "orders", env, CopySwapResume)
	require.ErrorIs(t, err, ErrInvalidOptions, "a resume with no store")
}
