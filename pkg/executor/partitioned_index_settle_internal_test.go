package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A walk that settles on its second pass stops there; one that keeps
// changing things stops at the limit with the caller's typed refusal, having
// walked exactly the passes it was allowed.
func TestSettlePassesStopsWhenAPassChangesNothing(t *testing.T) {
	passes, err := settlePasses(t.Context(), 5, ErrPartitionedIndexUnsettled, func(_ context.Context, pass int) (bool, error) {
		return pass == 1, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, passes)
}

func TestSettlePassesRefusesAWalkThatNeverSettles(t *testing.T) {
	for _, unsettled := range []error{ErrPartitionedIndexUnsettled, ErrPartitionedKeyUnsettled} {
		t.Run(unsettled.Error(), func(t *testing.T) {
			walked := 0
			passes, err := settlePasses(t.Context(), 3, unsettled, func(context.Context, int) (bool, error) {
				walked++
				return true, nil
			})
			assert.ErrorIs(t, err, unsettled)
			assert.Equal(t, 3, passes)
			assert.Equal(t, 3, walked)
		})
	}
}

func TestSettlePassesReturnsAWalkFailure(t *testing.T) {
	boom := errors.New("catalog read failed")
	passes, err := settlePasses(t.Context(), 3, ErrPartitionedIndexUnsettled, func(context.Context, int) (bool, error) {
		return false, boom
	})
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, 1, passes)
}
