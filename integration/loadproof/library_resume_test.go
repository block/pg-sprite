package loadproof_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// errNotResumed reports a call that ran the change again rather than
// carrying on the run the previous call stopped.
var errNotResumed = errors.New("the call did not resume the stopped run")

// checkResumed holds the call after a stop to the run the stop left: its
// new table is the shadow the stopped call built, so nothing was discarded
// and built again, and its copy carried on from where the stop left it.
// After a stop mid-copy, the first copying save it was handed carries a
// watermark at or above the one the row stood at, where a fresh start's
// carries none or a lower one; after a stop mid-verify, it saved no
// copying row at all, since the copy was already complete.
func checkResumed(stop stopPoint, stopped, resumed attemptInfo) error {
	if resumed.ShadowOID != stopped.ShadowOID {
		return fmt.Errorf("%w: attempt %d left its new table in relation %d, not %d, the shadow attempt %d built",
			errNotResumed, resumed.Attempt, resumed.ShadowOID, stopped.ShadowOID, stopped.Attempt)
	}
	switch stop {
	case stopMidCopy:
		return checkCopyContinued(stopped, resumed)
	case stopMidVerify:
		return checkCopyNotRepeated(resumed)
	default:
		return fmt.Errorf("%w: attempt %d follows no stop of the host's", errNotResumed, resumed.Attempt)
	}
}

// checkCopyContinued holds the call after a stop mid-copy to a copy that
// started from the stopped row's watermark.
func checkCopyContinued(stopped, resumed attemptInfo) error {
	if stopped.WatermarkAtStop == nil {
		return fmt.Errorf("%w: attempt %d records no watermark to resume from", errNotResumed, stopped.Attempt)
	}
	first, found := firstSaveIn(resumed.Saves, checkpoint.PhaseCopying)
	if !found {
		return fmt.Errorf("%w: attempt %d saved no copying row, so its copy is not shown to have carried on", errNotResumed, resumed.Attempt)
	}
	if first.Watermark == nil {
		return fmt.Errorf("%w: attempt %d's first copying save carries no watermark, as a fresh start's does", errNotResumed, resumed.Attempt)
	}
	if *first.Watermark < *stopped.WatermarkAtStop {
		return fmt.Errorf("%w: attempt %d's first copying save is at key %d, below %d, where attempt %d stopped",
			errNotResumed, resumed.Attempt, *first.Watermark, *stopped.WatermarkAtStop, stopped.Attempt)
	}
	return nil
}

// checkCopyNotRepeated holds the call after a stop mid-verify to a run
// that never went back to copying.
func checkCopyNotRepeated(resumed attemptInfo) error {
	if save, found := firstSaveIn(resumed.Saves, checkpoint.PhaseCopying); found {
		return fmt.Errorf("%w: attempt %d resumed a verifying row and saved a copying row %d ms in", errNotResumed, resumed.Attempt, save.AtMillis)
	}
	return nil
}

// firstSaveIn is the first save recorded in phase.
func firstSaveIn(saves []saveInfo, phase checkpoint.Phase) (saveInfo, bool) {
	for _, s := range saves {
		if s.Phase == phase.String() {
			return s, true
		}
	}
	return saveInfo{}, false
}

// relationOID is the OID of schema.name, an error when no relation has
// that name.
func relationOID(ctx context.Context, h host, schema, name string) (uint32, error) {
	var oid *uint32
	qualified := pgx.Identifier{schema, name}.Sanitize()
	if err := h.pool.QueryRow(ctx, `SELECT to_regclass($1)::oid`, qualified).Scan(&oid); err != nil {
		return 0, fmt.Errorf("read the OID of %s: %w", qualified, err)
	}
	if oid == nil {
		return 0, fmt.Errorf("no relation %s", qualified)
	}
	return *oid, nil
}

// stoppedWritesDeadline bounds the host's wait, between a cancelled call
// and the next, for the load to commit the writes the resume has to catch
// up on; a healthy load commits them within a tick or two.
const stoppedWritesDeadline = 30 * time.Second

// waitForStoppedWrites waits until the load has committed at least want
// writes beyond since, its count when the cancelled call returned, so the
// interval in which no call runs holds writes that only the slot the
// stopped run kept can carry into the new table. It returns how many the
// load committed beyond since.
func waitForStoppedWrites(ctx context.Context, load *testutil.LoadGenerator, since, want int) (int, error) {
	giveUp := time.NewTimer(stoppedWritesDeadline)
	defer giveUp.Stop()
	poll := time.NewTicker(commitPollInterval)
	defer poll.Stop()
	for {
		committed := load.Progress().Committed() - since
		if committed >= want {
			return committed, nil
		}
		select {
		case <-ctx.Done():
			return committed, fmt.Errorf("wait for the load's writes while no call runs: %w", ctx.Err())
		case <-giveUp.C:
			return committed, fmt.Errorf("the load committed %d writes, not %d, in %s while no call ran", committed, want, stoppedWritesDeadline)
		case <-poll.C:
		}
	}
}
