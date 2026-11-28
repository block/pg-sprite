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
	// killNever is the run the proof lets finish: the poll reads the row
	// throughout and sends nothing.
	killNever
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

// reached reports whether a poll that read cp and fill has seen the run at
// the kill point. Mid-copy is a row in copying carrying a saved watermark
// short of the key space, so the rerun has a save to resume from and the
// rows landed above it to clear, or, while the copy has saved nothing, a
// shadow at least half filled; mid-verify is a row in verifying, the pass
// that follows the copy and the catch-up. The run the proof lets finish is
// never reached.
func (k killPoint) reached(cp checkpoint.Checkpoint, fill copyFill) bool {
	switch k {
	case killMidCopy:
		return midCopyKill(cp, fill)
	case killMidVerify:
		return cp.Phase == checkpoint.PhaseVerifying
	default:
		return false
	}
}

// midCopyKill is killMidCopy's rule: the row in copying with a watermark
// saved and not complete, the copy's own record of how far it has landed.
// Only while the copy has saved nothing does the rule fall back to the
// heap sizes, the shadow at least half filled. A watermark short of the
// key space is below the table's highest key, since the chunk past it
// holds the keys above.
func midCopyKill(cp checkpoint.Checkpoint, fill copyFill) bool {
	if cp.Phase != checkpoint.PhaseCopying {
		return false
	}
	if cp.Watermark.Complete() {
		return false
	}
	if cp.Watermark.Valid() {
		return true
	}
	return fill.halfway()
}

// notReachedHint is what an operator can change when the binary ended
// before the poll saw the run at the kill point.
func (k killPoint) notReachedHint() string {
	switch k {
	case killMidCopy:
		return "the copy ended before the poll saw a save short of the key space or, with nothing saved, the shadow's heap at half the source's; " +
			"a source bloated by the load's updates keeps that ratio out of reach, so vacuum it or seed more rows (LOAD_PROOF_ROWS)"
	case killMidVerify:
		return "the verification pass ended between two polls; seed more rows (LOAD_PROOF_ROWS)"
	default:
		return "no kill was aimed at this run"
	}
}

// errRestartedCopy reports a rerun of a killed copy whose saves show it
// did not resume from the watermark the killed run saved.
var errRestartedCopy = errors.New("the rerun did not resume the copy from the killed run's save")

// checkResumedFrom holds the rerun of a killed copy to the first save the
// poll saw it make in copying, and reports whether it did. killedAt is the
// key the killed run's row had saved, nil when the kill landed before the
// copy's first save: the rerun then copies again from the first key, so
// there is nothing to hold it to and it reports false. Otherwise the rerun
// must have made a save the poll saw, at or above killedAt, since a copy
// resumed from killedAt lands only keys above it and a copy started over
// saves a watermark below it first.
func checkResumedFrom(killedAt *int64, firstSaved copier.Watermark) (bool, error) {
	if killedAt == nil {
		return false, nil
	}
	if !firstSaved.Valid() {
		return false, fmt.Errorf("%w: the poll saw no copying save from the rerun, so it cannot tell where the copy resumed; seed more rows (LOAD_PROOF_ROWS)", errRestartedCopy)
	}
	if firstSaved.Value() < *killedAt {
		return false, fmt.Errorf("%w: its first save is at key %d, below %d, where the killed run's save stood", errRestartedCopy, firstSaved.Value(), *killedAt)
	}
	return true, nil
}

// phaseNotSeen is the phase a poll that has read no row holds: the zero
// Phase, which no saved row carries.
const phaseNotSeen checkpoint.Phase = 0

// errVerifyNotResumed reports a rerun of a run killed in verifying that
// the poll did not first see past the copy.
var errVerifyNotResumed = errors.New("the rerun did not resume the verifying row")

// checkResumedPastCopy holds the rerun of a run killed in verifying to the
// first phase the poll saw it in: verifying or a later phase, since the
// copy was complete and a resume continues from the row's phase. A row
// first seen copying or catching up is a run that went back before the
// pass, and no row seen leaves the resume unproven.
func checkResumedPastCopy(first checkpoint.Phase) error {
	switch first {
	case checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone:
		return nil
	case phaseNotSeen:
		return fmt.Errorf("%w: the poll saw no row while the rerun ran, so it cannot tell where it resumed", errVerifyNotResumed)
	default:
		return fmt.Errorf("%w: the poll first saw the rerun's row in %s", errVerifyNotResumed, first)
	}
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
// was meant to: the binary exited on its own first, a SIGKILL the harness
// did not send ended it, or the row had moved past the phase the kill was
// aimed at.
var errNotKilled = errors.New("the run did not end where the harness killed it")

// checkKilledAt holds a killed run to the kill point: the harness sent the
// SIGKILL, the binary died of it rather than of its own accord or of a
// SIGKILL from elsewhere, and the checkpoint row it left stands in the
// phase the poll saw — for a kill mid-copy, with the copy not complete —
// so the kill landed inside the phase. stdout is the binary's, quoted when
// it exited on its own.
func checkKilledAt(point killPoint, cp checkpoint.Checkpoint, sent bool, signal syscall.Signal, code int, stdout string) error {
	if signal != syscall.SIGKILL {
		return fmt.Errorf("%w: the binary exited with code %d before the kill in %s landed; %s; stdout: %s",
			errNotKilled, code, point.phase(), point.notReachedHint(), stdout)
	}
	if !sent {
		return fmt.Errorf("%w: the binary died of a SIGKILL the harness did not send, such as the run budget's deadline or the out-of-memory killer", errNotKilled)
	}
	if cp.Phase != point.phase() {
		return fmt.Errorf("%w: the row stands in %s, not %s, where the binary was killed", errNotKilled, cp.Phase, point.phase())
	}
	if point == killMidCopy && cp.Watermark.Complete() {
		return fmt.Errorf("%w: the row in copying carries the complete watermark, so the copy had ended before the kill", errNotKilled)
	}
	return nil
}

// The binary is killed mid-copy as soon as the row carries a saved
// watermark short of the key space, however full the shadow's heap reads
// — a source bloated by the load's updates can keep the shadow below half
// its size — and, while the copy has saved nothing, once the shadow is
// half filled. A complete watermark or a row past copying is not killed.
func TestMidCopyKillPrefersTheSaveAndFallsBackToAHalfFilledShadow(t *testing.T) {
	half := copyFill{ShadowBytes: 4 << 20, SourceBytes: 8 << 20}
	quarter := copyFill{ShadowBytes: 2 << 20, SourceBytes: 8 << 20}
	partial := copier.NewWatermark(2002)
	cases := map[string]struct {
		cp   checkpoint.Checkpoint
		fill copyFill
		want bool
	}{
		"half filled, nothing saved yet":        {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, half, true},
		"a quarter filled, nothing saved yet":   {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, quarter, false},
		"no source read, nothing saved yet":     {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, copyFill{}, false},
		"half filled, a partial watermark":      {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: partial}, half, true},
		"a quarter filled, a partial watermark": {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: partial}, quarter, true},
		"a bloated source, a partial watermark": {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: partial}, copyFill{ShadowBytes: 1 << 20, SourceBytes: 80 << 20}, true},
		"half filled, the complete mark":        {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(math.MaxInt64)}, half, false},
		"catching up with the shadow full":      {checkpoint.Checkpoint{Phase: checkpoint.PhaseCatchingUp}, copyFill{ShadowBytes: 8 << 20, SourceBytes: 8 << 20}, false},
		"verifying with the shadow full":        {checkpoint.Checkpoint{Phase: checkpoint.PhaseVerifying}, copyFill{ShadowBytes: 8 << 20, SourceBytes: 8 << 20}, false},
		"the shadow larger than the source":     {checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, copyFill{ShadowBytes: 9 << 20, SourceBytes: 8 << 20}, true},
		"catching up with a partial watermark":  {checkpoint.Checkpoint{Phase: checkpoint.PhaseCatchingUp, Watermark: partial}, half, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, killMidCopy.reached(tc.cp, tc.fill))
		})
	}
}

// The binary is killed mid-verify at the first row in verifying, and at no
// other phase.
func TestMidVerifyKillIsTheVerifyingRow(t *testing.T) {
	full := copyFill{ShadowBytes: 8 << 20, SourceBytes: 8 << 20}
	for _, phase := range []checkpoint.Phase{checkpoint.PhaseCopying, checkpoint.PhaseCatchingUp, checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone} {
		t.Run(phase.String(), func(t *testing.T) {
			assert.Equal(t, phase == checkpoint.PhaseVerifying, killMidVerify.reached(checkpoint.Checkpoint{Phase: phase}, full))
		})
	}
}

// The run the proof lets finish is never killed, whatever the poll reads.
func TestTheFinishingRunIsNeverKilled(t *testing.T) {
	full := copyFill{ShadowBytes: 8 << 20, SourceBytes: 8 << 20}
	for _, phase := range []checkpoint.Phase{checkpoint.PhaseCopying, checkpoint.PhaseCatchingUp, checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone} {
		t.Run(phase.String(), func(t *testing.T) {
			assert.False(t, killNever.reached(checkpoint.Checkpoint{Phase: phase, Watermark: copier.NewWatermark(2002)}, full))
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

// checkKilledAt accepts a kill the harness sent that left the row in the
// phase it was aimed at, a copy killed before its first save included, and
// refuses a binary that exited on its own, one a SIGKILL the harness did
// not send ended, and a row that had moved on or finished its copy.
func TestCheckKilledAtHoldsTheKillInsideItsPhase(t *testing.T) {
	copying := checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(2002)}
	verifying := checkpoint.Checkpoint{Phase: checkpoint.PhaseVerifying, Watermark: copier.NewWatermark(math.MaxInt64)}
	require.NoError(t, checkKilledAt(killMidCopy, copying, true, syscall.SIGKILL, -1, ""))
	require.NoError(t, checkKilledAt(killMidCopy, checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying}, true, syscall.SIGKILL, -1, ""))
	require.NoError(t, checkKilledAt(killMidVerify, verifying, true, syscall.SIGKILL, -1, ""))

	refused := map[string]struct {
		point  killPoint
		cp     checkpoint.Checkpoint
		sent   bool
		signal syscall.Signal
	}{
		"exited on its own":                 {killMidVerify, checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}, false, 0},
		"exited as the kill was sent":       {killMidVerify, checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}, true, 0},
		"ended by another signal":           {killMidVerify, verifying, true, syscall.SIGTERM},
		"a copy killed by someone else":     {killMidCopy, copying, false, syscall.SIGKILL},
		"a verify pass killed by the clock": {killMidVerify, verifying, false, syscall.SIGKILL},
		"the copy had moved on":             {killMidCopy, checkpoint.Checkpoint{Phase: checkpoint.PhaseCatchingUp}, true, syscall.SIGKILL},
		"the copy had completed":            {killMidCopy, checkpoint.Checkpoint{Phase: checkpoint.PhaseCopying, Watermark: copier.NewWatermark(math.MaxInt64)}, true, syscall.SIGKILL},
		"the verify pass had cut over":      {killMidVerify, checkpoint.Checkpoint{Phase: checkpoint.PhaseCutover}, true, syscall.SIGKILL},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, checkKilledAt(tc.point, tc.cp, tc.sent, tc.signal, 0, `{"outcome":"executed-via-copy-and-swap"}`), errNotKilled)
		})
	}
}

// The rerun of a copy killed with key 2002 saved resumed it when its first
// save the poll saw is at or above 2002, the complete watermark included;
// a first save below 2002 is a copy started over, and no save seen leaves
// the resume unproven. A copy killed before its first save leaves nothing
// to hold the rerun to, which is reported as not proven rather than as
// proven or failed.
func TestCheckResumedFromHoldsTheRerunToTheKilledSave(t *testing.T) {
	killedAt := int64(2002)
	for name, firstSaved := range map[string]copier.Watermark{
		"above the save":         copier.NewWatermark(4004),
		"at the save":            copier.NewWatermark(killedAt),
		"the complete watermark": copier.NewWatermark(math.MaxInt64),
	} {
		t.Run(name, func(t *testing.T) {
			proven, err := checkResumedFrom(&killedAt, firstSaved)
			require.NoError(t, err)
			assert.True(t, proven)
		})
	}
	for name, firstSaved := range map[string]copier.Watermark{
		"below the save": copier.NewWatermark(1001),
		"no save seen":   {},
	} {
		t.Run(name, func(t *testing.T) {
			proven, err := checkResumedFrom(&killedAt, firstSaved)
			require.ErrorIs(t, err, errRestartedCopy)
			assert.False(t, proven)
		})
	}
	t.Run("killed before the first save", func(t *testing.T) {
		for _, firstSaved := range []copier.Watermark{{}, copier.NewWatermark(1001), copier.NewWatermark(4004)} {
			proven, err := checkResumedFrom(nil, firstSaved)
			require.NoError(t, err)
			assert.False(t, proven, "a rerun with no save to resume from is not shown to have resumed from one")
		}
	})
}

// The rerun of a run killed in verifying is first seen in verifying or a
// later phase; a first sighting in copying or catching up is a run that
// went back before the pass, and no sighting proves nothing.
func TestCheckResumedPastCopyHoldsTheRerunToTheVerifyingRow(t *testing.T) {
	for _, phase := range []checkpoint.Phase{checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone} {
		t.Run(phase.String(), func(t *testing.T) {
			require.NoError(t, checkResumedPastCopy(phase))
		})
	}
	for _, phase := range []checkpoint.Phase{phaseNotSeen, checkpoint.PhaseCopying, checkpoint.PhaseCatchingUp, checkpoint.PhaseReconciling, checkpoint.PhaseFailed} {
		t.Run(phase.String(), func(t *testing.T) {
			require.ErrorIs(t, checkResumedPastCopy(phase), errVerifyNotResumed)
		})
	}
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
