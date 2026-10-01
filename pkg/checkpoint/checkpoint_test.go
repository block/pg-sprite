package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhaseString(t *testing.T) {
	cases := map[Phase]string{PhaseCopying: "copying", PhaseCatchingUp: "catching_up", PhaseVerifying: "verifying", PhaseCutover: "cutover", PhaseDone: "done", PhaseFailed: "failed", Phase(99): "Phase(99)"}
	for phase, want := range cases {
		assert.Equal(t, want, phase.String())
	}
}

func TestPhaseTerminal(t *testing.T) {
	terminal := map[Phase]bool{PhaseCopying: false, PhaseCatchingUp: false, PhaseVerifying: false, PhaseCutover: false, PhaseDone: true, PhaseFailed: true, Phase(0): false}
	for phase, want := range terminal {
		assert.Equal(t, want, phase.Terminal(), "phase %s", phase)
	}
}

// Every phase name String produces parses back to its Phase; names String
// never produces, including the unknown-phase rendering, do not.
func TestParsePhaseInvertsString(t *testing.T) {
	for p := PhaseCopying; p <= PhaseFailed; p++ {
		got, err := parsePhase(p.String())
		require.NoError(t, err, p)
		assert.Equal(t, p, got)
	}
	for _, bad := range []string{"", "Copying", "paused", "Phase(99)"} {
		_, err := parsePhase(bad)
		assert.Error(t, err, bad)
	}
}

func validCheckpoint() Checkpoint {
	return Checkpoint{
		Schema: "app", Table: "orders", ShadowTable: "_pgsprite_orders_new",
		SourceFingerprint: "src-a", TargetFingerprint: "tgt-a", Phase: PhaseCopying,
	}
}

// The key, the shadow, the fingerprints, and the phase are what resume keys
// on, so a checkpoint missing any of them is refused before it reaches the
// database; the slot and publication names may be empty because a quiesced
// run has none.
func TestCheckpointValidate(t *testing.T) {
	require.NoError(t, validCheckpoint().validate())

	cases := map[string]func(*Checkpoint){
		"empty schema":             func(c *Checkpoint) { c.Schema = "" },
		"empty table":              func(c *Checkpoint) { c.Table = "" },
		"empty shadow table":       func(c *Checkpoint) { c.ShadowTable = "" },
		"empty source fingerprint": func(c *Checkpoint) { c.SourceFingerprint = "" },
		"empty target fingerprint": func(c *Checkpoint) { c.TargetFingerprint = "" },
		"zero phase":               func(c *Checkpoint) { c.Phase = 0 },
		"unknown phase":            func(c *Checkpoint) { c.Phase = Phase(99) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cp := validCheckpoint()
			mutate(&cp)
			assert.ErrorIs(t, cp.validate(), ErrInvalidCheckpoint)
		})
	}
}

func TestCheckpointFingerprints(t *testing.T) {
	assert.Equal(t, Fingerprints{Source: "src-a", Target: "tgt-a"}, validCheckpoint().Fingerprints())
}
