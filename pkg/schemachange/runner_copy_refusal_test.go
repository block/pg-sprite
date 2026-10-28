package schemachange

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// Every copier cause is re-minted as a shadow cause that upholds the same
// invariant and routes to the same class, so an importer reading either
// vocabulary reacts the same way: a copier cause added without a mapping,
// or mapped across invariants or classes, fails here.
func TestEveryCopierCauseIsRemintedUnderTheSameInvariant(t *testing.T) {
	assert.Empty(t, copierCause(""), "the empty copier cause is no refusal")
	assert.Empty(t, copierCause("not-a-cause"))
	for _, cause := range copier.RefusalCauses() {
		mapped := copierCause(cause)
		require.NotEmpty(t, mapped, "copier cause %q has no shadow cause", cause)
		assert.Contains(t, RefusalCauses(), mapped, "copier cause %q maps to unregistered %q", cause, mapped)
		assert.Equal(t, cause.Invariant(), mapped.Invariant(), "copier cause %q and %q uphold different invariants", cause, mapped)
		assert.Equal(t, cause.Class(), mapped.Class(), "copier cause %q and %q route to different classes", cause, mapped)
	}
}

// A re-minted copier refusal answers in both vocabularies and still
// reaches the copier's own refusal; an error that is not a copier refusal
// passes through untouched.
func TestCopyRefusalRemintsCopierRefusalsOnly(t *testing.T) {
	copierErr := &copier.RefusalError{Cause: copier.CauseLockLost, Detail: "table lock was lost during chunk [1,1000)"}
	err := copyRefusal(copierErr, "sales", "widgets")
	assert.Equal(t, CauseLockLost, RefusalCauseOf(err))
	assert.Equal(t, copier.CauseLockLost, copier.RefusalCauseOf(err))
	assert.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorIs(t, err, copierErr)
	assert.Contains(t, err.Error(), "copy of sales.widgets refused")
	assert.Contains(t, err.Error(), copierErr.Detail)

	server := errors.New("connection reset")
	assert.Same(t, server, copyRefusal(server, "sales", "widgets"))
	assert.NoError(t, copyRefusal(nil, "sales", "widgets"))
}

// Each copier cause is re-minted as the one shadow cause named for it, not
// merely as one that upholds the same invariant: within an invariant the
// causes still call for different reactions, so a swap is a wrong answer.
func TestCopierCausesRemintAsTheNamedShadowCause(t *testing.T) {
	want := map[copier.RefusalCause]RefusalCause{
		copier.CauseProofEmpty:        CauseProofEmpty,
		copier.CauseShadowMismatch:    CauseCopyShadowMismatch,
		copier.CauseLockUnproven:      CauseLockUnproven,
		copier.CauseLockLost:          CauseLockLost,
		copier.CauseLockHeldElsewhere: CauseLockHeldElsewhere,
		copier.CauseLockUnconfirmed:   CauseLockUnconfirmed,
		copier.CauseRelationReplaced:  CauseRelationReplaced,
		copier.CauseConversionDrift:   CauseCopyConversionDrift,
		copier.CauseFrontierBroken:    CauseCopyFrontierBroken,
		copier.CauseChunksInFlight:    CauseCopyChunksInFlight,
		copier.CauseChunkUnknown:      CauseCopyChunkUnknown,
	}
	assert.Len(t, want, len(copier.RefusalCauses()))
	for _, cause := range copier.RefusalCauses() {
		assert.Equal(t, want[cause], copierCause(cause), "copier cause %q", cause)
	}
}

// The re-mint column of the copier table in docs/refusal-classes.md is the
// contract an importer reads; it names, for every copier cause, the cause
// copierCause answers with.
func TestRefusalClassesDocRemintColumnMatchesCopierCause(t *testing.T) {
	raw, err := os.ReadFile("../../docs/refusal-classes.md")
	require.NoError(t, err)
	remints := copierTableRemints(t, string(raw))
	require.Len(t, remints, len(copier.RefusalCauses()), "one copier table row per copier cause")
	for _, cause := range copier.RefusalCauses() {
		assert.Equal(t, string(copierCause(cause)), remints[string(cause)], "re-mint column for copier cause %q", cause)
	}
}

// copierTableRemints reads the copier refusal table of the refusal-class
// map into cause -> re-minted cause, from the table's first and last
// columns. Only the rows of the copier table are read: the shadow table
// above it lists the same `copy-*` names under its own meaning.
func copierTableRemints(t *testing.T, doc string) map[string]string {
	t.Helper()
	const heading = "### Copier refusals, keyed on `copier.RefusalCause`"
	start := strings.Index(doc, heading)
	require.NotEqual(t, -1, start, "docs/refusal-classes.md has the copier section")
	section := doc[start+len(heading):]
	if end := strings.Index(section, "\n### "); end != -1 {
		section = section[:end]
	}
	remints := make(map[string]string)
	for line := range strings.SplitSeq(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 7 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`copy-") {
			continue
		}
		cause := strings.Trim(strings.TrimSpace(cells[1]), "`")
		remints[cause] = strings.Trim(strings.TrimSpace(cells[5]), "`")
	}
	return remints
}

// A copy whose periodic save failed reports the save's failure, and keeps
// a refusal the copier reached on its own in front of it, re-minted; a
// copier that stopped on the save's own cancellation adds nothing.
func TestCopyStoppedKeepsACopierRefusalBesideTheSaveFailure(t *testing.T) {
	saveErr := errors.New("save checkpoint for sales.widgets: connection reset")
	copierErr := &copier.RefusalError{Cause: copier.CauseLockLost, Detail: "table lock was lost during the copy"}

	err := copyStopped(copierErr, saveErr, "sales", "widgets")
	assert.Equal(t, CauseLockLost, RefusalCauseOf(err))
	assert.Equal(t, copier.CauseLockLost, copier.RefusalCauseOf(err))
	assert.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorIs(t, err, copierErr)
	assert.ErrorIs(t, err, saveErr)

	assert.Same(t, saveErr, copyStopped(saveErr, saveErr, "sales", "widgets"), "the copier stopped on the save's cancellation")
	assert.Same(t, saveErr, copyStopped(nil, saveErr, "sales", "widgets"), "the copier finished as the save failed")
	assert.Empty(t, RefusalCauseOf(copyStopped(saveErr, saveErr, "sales", "widgets")))
}
