package migrate

import (
	"testing"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/refusal"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The route's cause rides on the verdict unchanged under the one reason,
// and the class follows the cause: an operator-clearable preflight cause is
// environmental, a shape the route does not carry is a capability boundary.
func TestCopySwapPreflightRefusalCarriesCauseAndClass(t *testing.T) {
	tests := []struct {
		cause preflight.CopySwapRefusalCause
		class verdict.Class
	}{
		{preflight.CopySwapCauseDiskHeadroom, verdict.ClassEnvironmental},
		{preflight.CopySwapCauseReplicaIdentity, verdict.ClassEnvironmental},
		{preflight.CopySwapCauseLogicalDecodingUnavailable, verdict.ClassEnvironmental},
		{preflight.CopySwapCauseUnlogged, verdict.ClassCapabilityBoundary},
		{preflight.CopySwapCauseForeignKeys, verdict.ClassCapabilityBoundary},
		{preflight.CopySwapCausePKUnsupported, verdict.ClassCapabilityBoundary},
	}
	for _, tc := range tests {
		t.Run(string(tc.cause), func(t *testing.T) {
			r, ok := copySwapPreflightRefusal(tc.cause)
			require.True(t, ok)
			assert.Equal(t, verdict.ReasonCopyAndSwapRefused, r.Reason())
			assert.Equal(t, tc.class, r.Class())
			assert.Equal(t, verdict.Cause(tc.cause), r.Cause())
			assert.Empty(t, r.Owner())
		})
	}
	_, ok := copySwapPreflightRefusal("")
	assert.False(t, ok, "the zero cause is not a refusal")
	_, ok = copySwapPreflightRefusal("copy-and-swap-brand-new")
	assert.False(t, ok, "an unlisted cause fails closed")
}

// A shadow refusal takes the class its cause declares, token for token, and
// a cause that declares no class is left unclassified rather than defaulted.
func TestShadowRefusalFollowsTheCausesOwnClass(t *testing.T) {
	for _, cause := range schemachange.RefusalCauses() {
		t.Run(string(cause), func(t *testing.T) {
			r, ok := shadowRefusal(cause)
			require.True(t, ok)
			assert.Equal(t, verdict.ReasonCopyAndSwapRefused, r.Reason())
			assert.Equal(t, verdict.Class(cause.Class()), r.Class())
			assert.Equal(t, verdict.Cause(cause), r.Cause())
		})
	}
	_, ok := shadowRefusal("shadow-brand-new")
	assert.False(t, ok)
}

// Every class the core can hand out has a verdict counterpart of the same
// token; the fourth and fifth verdict classes have no core counterpart and
// are never minted from a core cause.
func TestRefusalOfClassConvertsByToken(t *testing.T) {
	for _, class := range refusal.Classes() {
		r, ok := refusalOfClass(class, verdict.ReasonCopyAndSwapRefused)
		require.True(t, ok, class)
		assert.Equal(t, string(class), string(r.Class()))
		_, err := verdict.ParseClass(string(r.Class()))
		assert.NoError(t, err)
	}
	_, ok := refusalOfClass("", verdict.ReasonCopyAndSwapRefused)
	assert.False(t, ok)
}

// A copy-and-swap refusal is never an accepted-blocking candidate: the route
// has no blocking form to fall back to.
func TestCopySwapRefusalsAreNotAcceptedBlockingEligible(t *testing.T) {
	for _, cause := range preflight.CopySwapRefusalCauses() {
		r, ok := copySwapPreflightRefusal(cause)
		require.True(t, ok, cause)
		eligible, decided := verdict.AcceptedBlockingDecision(r)
		assert.False(t, eligible, cause)
		assert.True(t, decided, cause)
	}
	for _, cause := range schemachange.RefusalCauses() {
		r, ok := shadowRefusal(cause)
		require.True(t, ok, cause)
		eligible, decided := verdict.AcceptedBlockingDecision(r)
		assert.False(t, eligible, cause)
		assert.True(t, decided, cause)
	}
}
