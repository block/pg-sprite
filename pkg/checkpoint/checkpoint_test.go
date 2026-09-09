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
