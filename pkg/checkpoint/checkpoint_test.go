package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
