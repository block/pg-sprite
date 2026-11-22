package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A walk that settles on its second pass stops there; one that keeps
// changing things stops at the limit with the typed refusal, having walked
// exactly the passes it was allowed.
func TestSettlePassesStopsWhenAPassChangesNothing(t *testing.T) {
	passes, err := settlePasses(t.Context(), 5, func(_ context.Context, pass int) (bool, error) {
		return pass == 1, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, passes)
}

func TestSettlePassesRefusesAWalkThatNeverSettles(t *testing.T) {
	walked := 0
	passes, err := settlePasses(t.Context(), 3, func(context.Context, int) (bool, error) {
		walked++
		return true, nil
	})
	assert.ErrorIs(t, err, ErrPartitionedIndexUnsettled)
	assert.Equal(t, 3, passes)
	assert.Equal(t, 3, walked)
}

func TestSettlePassesReturnsAWalkFailure(t *testing.T) {
	boom := errors.New("catalog read failed")
	passes, err := settlePasses(t.Context(), 3, func(context.Context, int) (bool, error) {
		return false, boom
	})
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, 1, passes)
}
