package schemachange

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/progress"
)

// The reconcile's clear of the shadow is bounded exactly as its pass is:
// the same lock_timeout and statement_timeout on every batch (LK-2), the
// same chunker sizing every batch, and the same clock timing them. A
// bound the clear did not carry would fall back to the copier's default
// and leave the operator's budget for the pass unenforced on the clear
// that precedes it. Workers and the tracker have no bearing on a clear
// that runs one batch at a time and reports no progress, and are left
// unset.
func TestClearOptionsCarryThePassBounds(t *testing.T) {
	clock := progress.WallClock{}
	pass := checksum.Options{
		LockTimeout:      1500 * time.Millisecond,
		StatementTimeout: 7 * time.Second,
		Chunker:          copier.ChunkerOptions{InitialRows: 50, MinRows: 25, MaxRows: 75, TargetChunkTime: 250 * time.Millisecond},
		Clock:            clock,
		RecopyAttempts:   4,
	}

	assert.Equal(t, copier.Options{
		LockTimeout:      1500 * time.Millisecond,
		StatementTimeout: 7 * time.Second,
		Chunker:          copier.ChunkerOptions{InitialRows: 50, MinRows: 25, MaxRows: 75, TargetChunkTime: 250 * time.Millisecond},
		Clock:            clock,
	}, clearOptions(pass))
}
