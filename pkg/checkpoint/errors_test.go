package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Each Mismatch has a stable name an operator reads in an
// IncompatibleError; a value outside the set renders as its number.
func TestMismatchString(t *testing.T) {
	cases := map[Mismatch]string{
		MismatchFormat: "format_version",
		MismatchSource: "source_fingerprint",
		MismatchTarget: "target_fingerprint",
		MismatchPhase:  "phase",
		Mismatch(99):   "Mismatch(99)",
	}
	for mismatch, want := range cases {
		assert.Equal(t, want, mismatch.String())
	}
}
