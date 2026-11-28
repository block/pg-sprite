package loadproof_test

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
)

// killPoint is where the kill proof sends the binary SIGKILL.
type killPoint int

const (
	killMidCopy killPoint = iota + 1
	killMidVerify
)

// killPoints are the kill proof's kills, in order, before the run it lets
// finish.
func killPoints() []killPoint { return []killPoint{killMidCopy, killMidVerify} }

// phase is the checkpoint phase the row must still stand in after the
// kill, so the kill landed inside it rather than after it.
func (k killPoint) phase() checkpoint.Phase {
	switch k {
	case killMidCopy:
		return checkpoint.PhaseCopying
	case killMidVerify:
		return checkpoint.PhaseVerifying
	default:
		return checkpoint.PhaseDone
	}
}

// copyFill is the heap size, in bytes, of the shadow the copy is filling
// and of the source it copies from, as one poll read them.
type copyFill struct {
	ShadowBytes int64
	SourceBytes int64
}

// halfway reports whether the shadow's heap has reached half the source's:
// the copy has landed rows well into the key space, whether or not its
// first periodic save has recorded a watermark yet.
func (f copyFill) halfway() bool {
	return f.SourceBytes > 0 && f.ShadowBytes*2 >= f.SourceBytes
}

// reached reports whether a poll that read cp and fill, with verify the
// last verify progress the run printed, has seen the run at the kill
// point. Mid-copy is a row in copying carrying a saved watermark short of
// the key space, with the shadow at least half filled, so the rerun has a
// save to resume from and rows above it to clear; mid-verify is a row in
// verifying whose pass has printed a percent above zero, so it has
// compared at least one chunk.
func (k killPoint) reached(cp checkpoint.Checkpoint, fill copyFill, verify verifySeen) bool {
	switch k {
	case killMidCopy:
		return midCopyKill(cp, fill)
	case killMidVerify:
		return midVerifyKill(cp, verify)
	default:
		return false
	}
}

// midVerifyKill is killMidVerify's rule: the row in verifying and the
// pass's progress line past 0%.
func midVerifyKill(cp checkpoint.Checkpoint, verify verifySeen) bool {
	return cp.Phase == checkpoint.PhaseVerifying && verify.compared()
}

// midCopyKill is killMidCopy's rule: the row in copying, a watermark saved
// and not complete, and the shadow at least half filled. A watermark short
// of the key space is below the table's highest key, since the chunk past
// it holds the keys above.
func midCopyKill(cp checkpoint.Checkpoint, fill copyFill) bool {
	if cp.Phase != checkpoint.PhaseCopying {
		return false
	}
	if !partialWatermark(cp.Watermark) {
		return false
	}
	return fill.halfway()
}

// partialWatermark reports whether w is a saved watermark short of the key
// space.
func partialWatermark(w copier.Watermark) bool {
	return w.Valid() && !w.Complete()
}

// errRestartedCopy reports a rerun of a killed copy whose saves show it
// did not resume from the watermark the killed run saved.
var errRestartedCopy = errors.New("the rerun did not resume the copy from the killed run's save")

// checkResumedFrom holds the rerun of a copy killed with killedAt saved to
// the first save the poll saw it make: there must be one, and it must be
// at or above killedAt, since a copy resumed from killedAt lands only keys
// above it and a copy started over saves a watermark below it first.
func checkResumedFrom(killedAt int64, firstSaved copier.Watermark) error {
	if !firstSaved.Valid() {
		return fmt.Errorf("%w: the poll saw no copying save from the rerun, so it cannot tell where the copy resumed; seed more rows (LOAD_PROOF_ROWS)", errRestartedCopy)
	}
	if firstSaved.Value() < killedAt {
		return fmt.Errorf("%w: its first save is at key %d, below %d, where the killed run's save stood", errRestartedCopy, firstSaved.Value(), killedAt)
	}
	return nil
}

// errCopyReentered reports a run that resumed past the copy and copied
// again.
var errCopyReentered = errors.New("a run resumed past the copy entered copying again")

// checkNoCopyAfterVerify holds the phases the checkpoint poll saw, in the
// order it saw them across every run, to the resume rule for a verifying
// row: once the row has stood in verifying, no run copies again.
func checkNoCopyAfterVerify(phases []checkpoint.Phase) error {
	verified := false
	for i, p := range phases {
		switch {
		case p == checkpoint.PhaseVerifying:
			verified = true
		case verified && p == checkpoint.PhaseCopying:
			return fmt.Errorf("%w: the poll saw %v, copying at position %d after verifying", errCopyReentered, phases, i)
		}
	}
	return nil
}

// exitSignal is the signal that ended the process, zero when it exited on
// its own.
func exitSignal(state *os.ProcessState) syscall.Signal {
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0
	}
	return ws.Signal()
}

// errNotKilled reports a run the harness's SIGKILL did not end where it
// was meant to: the binary exited on its own first, or the row had moved
// past the phase the kill was aimed at.
var errNotKilled = errors.New("the run did not end where the harness killed it")

// checkKilledAt holds a killed run to the kill point: the binary died of
// SIGKILL, not of its own accord, and the checkpoint row it left stands in
// the phase the poll saw — for a kill mid-copy, with a saved watermark
// short of the key space, for a kill mid-verify, with a progress line at
// the kill saying the pass had compared a chunk — so the kill landed inside
// the phase. stdout is the binary's, quoted when it exited on its own.
func checkKilledAt(point killPoint, cp checkpoint.Checkpoint, verify verifySeen, signal syscall.Signal, code int, stdout string) error {
	if signal != syscall.SIGKILL {
		return fmt.Errorf("%w: the binary exited with code %d before the kill in %s landed; seed more rows (LOAD_PROOF_ROWS); stdout: %s",
			errNotKilled, code, point.phase(), stdout)
	}
	if cp.Phase != point.phase() {
		return fmt.Errorf("%w: the row stands in %s, not %s, where the binary was killed", errNotKilled, cp.Phase, point.phase())
	}
	if point == killMidCopy && cp.Watermark.Complete() {
		return fmt.Errorf("%w: the row in copying carries the complete watermark, so the copy had ended before the kill", errNotKilled)
	}
	if point == killMidCopy && !cp.Watermark.Valid() {
		return fmt.Errorf("%w: the row in copying carries no watermark, so the killed copy saved nothing to resume from", errNotKilled)
	}
	if point == killMidVerify && !verify.compared() {
		return fmt.Errorf("%w: the last verify progress line before the kill was %q, so the pass had compared no chunk", errNotKilled, verify.Line)
	}
	return nil
}

// The binary is killed mid-copy only once the shadow is half filled and
// the row carries a saved watermark short of the key space, so the rerun
// has a save to resume from: a row with nothing saved yet is not killed.
func TestMidCopyKillNeedsAHalfFilledShadowAndAPartialSave(t *testing.T) {
	half := copyFill{ShadowBytes: 4 << 20, SourceBytes: 8 << 20}
	partial := copier.NewWatermark(2002)
	cases := map[string]struct {
		cp   checkpoint.Checkpoint
		fill copyFill
		want bool
	}{
		"half filled, nothing saved yet":    {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, half, false},
		"half filled, a partial watermark":  {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: partial}, half, true},
		"half filled, the complete mark":    {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(math.MaxInt64)}, half, false},
		"a quarter filled":                  {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: partial}, copyFill{ShadowBytes: 2 << 20, SourceBytes: 8 << 20}, false},
		"no source read":                    {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: partial}, copyFill{}, false},
		"catching up with the shadow full":  {checkpoint.Checkpoint{Phase: checkpoint.PhaseCatchingUp}, copyFill{ShadowBytes: 8 << 20, SourceBytes: 8 << 20}, false},
		"verifying with the shadow full":    {checkpoint.Checkpoint{Phase: checkpoint.PhaseVerifying}, copyFill{ShadowBytes: 8 << 20, SourceBytes: 8 << 20}, false},
		"the shadow larger than the source": {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: partial}, copyFill{ShadowBytes: 9 << 20, SourceBytes: 8 << 20}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, killMidCopy.reached(tc.cp, tc.fill, verifySeen{}))
		})
	}
}

// The binary is killed mid-verify only on a row in verifying whose pass
// has printed a percent above zero: a row in verifying with no progress
// line yet, or one at 0%, has compared no chunk, and a row in any other
// phase is not the verify pass, whatever line was printed last.
func TestMidVerifyKillNeedsTheVerifyingRowAndAComparedChunk(t *testing.T) {
	full := copyFill{ShadowBytes: 8 << 20, SourceBytes: 8 << 20}
	compared := verifySeen{Percent: 15, Line: "step 4/6  checksum  15.00%  3.0K/20.0K rows"}
	cases := map[string]struct {
		phase  checkpoint.Phase
		verify verifySeen
		want   bool
	}{
		"verifying, no progress line yet":    {checkpoint.PhaseVerifying, verifySeen{}, false},
		"verifying, the pass at 0%":          {checkpoint.PhaseVerifying, verifySeen{Percent: 0, Line: "step 4/6  checksum  0.00%  0/20.0K rows"}, false},
		"verifying, a chunk compared":        {checkpoint.PhaseVerifying, compared, true},
		"copying, a chunk compared":          {checkpoint.PhaseCopying, compared, false},
		"catching up, a chunk compared":      {checkpoint.PhaseCatchingUp, compared, false},
		"cutover after a chunk was compared": {checkpoint.PhaseCutover, compared, false},
		"done after a chunk was compared":    {checkpoint.PhaseDone, compared, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, killMidVerify.reached(checkpoint.Checkpoint{Phase: tc.phase}, full, tc.verify))
		})
	}
}

// The proof kills mid-copy, then mid-verify, before the run it lets
// finish, and each kill's row must stand in the phase it was aimed at.
func TestKillPointsAreMidCopyThenMidVerify(t *testing.T) {
	assert.Equal(t, []killPoint{killMidCopy, killMidVerify}, killPoints())
	assert.Equal(t, checkpoint.PhaseCopying, killMidCopy.phase())
	assert.Equal(t, checkpoint.PhaseVerifying, killMidVerify.phase())
}

// exitSignal reads SIGKILL off a process killed with it, and nothing off
// one that exited on its own, even with a failing code.
func TestExitSignalTellsAKillFromAnExit(t *testing.T) {
	shell, err := exec.LookPath("sh")
	require.NoError(t, err)

	killed := exec.CommandContext(t.Context(), shell, "-c", "kill -KILL $$")
	var exit *exec.ExitError
	require.ErrorAs(t, killed.Run(), &exit)
	assert.Equal(t, syscall.SIGKILL, exitSignal(killed.ProcessState))

	failed := exec.CommandContext(t.Context(), shell, "-c", "exit 3")
	require.ErrorAs(t, failed.Run(), &exit)
	assert.Equal(t, syscall.Signal(0), exitSignal(failed.ProcessState))
	assert.Equal(t, 3, failed.ProcessState.ExitCode())
}

// checkKilledAt accepts a kill that left the row in the phase it was aimed
// at, and refuses a binary that exited on its own and a row that had moved
// on or finished its copy, and a kill mid-verify before the pass had
// printed a percent above zero.
func TestCheckKilledAtHoldsTheKillInsideItsPhase(t *testing.T) {
	copying := checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(2002)}
	verifying := checkpoint.Checkpoint{Phase: checkpoint.PhaseVerifying, Watermark: copier.NewWatermark(math.MaxInt64)}
	compared := verifySeen{Percent: 15, Line: "step 4/6  checksum  15.00%  3.0K/20.0K rows"}
	require.NoError(t, checkKilledAt(killMidCopy, copying, verifySeen{}, syscall.SIGKILL, -1, ""))
	require.NoError(t, checkKilledAt(killMidVerify, verifying, compared, syscall.SIGKILL, -1, ""))

	refused := map[string]struct {
		point  killPoint
		cp     checkpoint.Checkpoint
		verify verifySeen
		signal syscall.Signal
	}{
		"exited on its own":                  {killMidVerify, checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}, compared, 0},
		"ended by another signal":            {killMidVerify, verifying, compared, syscall.SIGTERM},
		"the copy had moved on":              {killMidCopy, checkpoint.Checkpoint{Phase: checkpoint.PhaseCatchingUp}, verifySeen{}, syscall.SIGKILL},
		"the copy had completed":             {killMidCopy, checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(math.MaxInt64)}, verifySeen{}, syscall.SIGKILL},
		"the copy had saved nothing":         {killMidCopy, checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, verifySeen{}, syscall.SIGKILL},
		"the verify pass had cut over":       {killMidVerify, checkpoint.Checkpoint{Phase: checkpoint.PhaseCutover}, compared, syscall.SIGKILL},
		"the verify pass printed no percent": {killMidVerify, verifying, verifySeen{}, syscall.SIGKILL},
		"the verify pass was at 0%":          {killMidVerify, verifying, verifySeen{Percent: 0, Line: "step 4/6  checksum  0.00%  0/20.0K rows"}, syscall.SIGKILL},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, checkKilledAt(tc.point, tc.cp, tc.verify, tc.signal, 0, `{"outcome":"executed-via-copy-and-swap"}`), errNotKilled)
		})
	}
}

// The rerun of a copy killed with key 2002 saved resumed it when its first
// save the poll saw is at or above 2002, the complete watermark included;
// a first save below 2002 is a copy started over, and no save seen leaves
// the resume unproven.
func TestCheckResumedFromHoldsTheRerunToTheKilledSave(t *testing.T) {
	const killedAt = 2002
	require.NoError(t, checkResumedFrom(killedAt, copier.NewWatermark(4004)))
	require.NoError(t, checkResumedFrom(killedAt, copier.NewWatermark(killedAt)))
	require.NoError(t, checkResumedFrom(killedAt, copier.NewWatermark(math.MaxInt64)))
	require.ErrorIs(t, checkResumedFrom(killedAt, copier.NewWatermark(1001)), errRestartedCopy)
	require.ErrorIs(t, checkResumedFrom(killedAt, copier.Watermark{}), errRestartedCopy)
}

// Once the poll has seen the row in verifying, a later sighting of copying
// is a run that copied again; the phases before the first verifying, and
// every other phase after it, are allowed.
func TestCheckNoCopyAfterVerifyRefusesACopyAfterVerifying(t *testing.T) {
	resumed := []checkpoint.Phase{
		checkpoint.PhaseCopying, checkpoint.PhaseCatchingUp, checkpoint.PhaseVerifying,
		checkpoint.PhaseCatchingUp, checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone,
	}
	require.NoError(t, checkNoCopyAfterVerify(resumed))
	recopied := []checkpoint.Phase{
		checkpoint.PhaseCopying, checkpoint.PhaseVerifying, checkpoint.PhaseCopying, checkpoint.PhaseVerifying,
	}
	require.ErrorIs(t, checkNoCopyAfterVerify(recopied), errCopyReentered)
}
