package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The text of a discarded live run names each leftover it dropped, in the
// order the discard drops them, so an operator can see the slot is gone.
func TestRenderDiscardNamesWhatWasDropped(t *testing.T) {
	text := renderDiscard(discardReport{
		Table:       "public.orders",
		Outcome:     discardOutcomeDiscarded,
		RowPhase:    "catching_up",
		Shadow:      "_pgsprite_orders_new",
		Slot:        "pgsprite_b32f988b",
		Publication: "pgsprite_b32f988b",
		schema:      "public",
	})
	assert.Equal(t, "discarded the stopped copy-and-swap run on public.orders; its change was not run\n"+
		"  dropped shadow table public._pgsprite_orders_new\n"+
		"  dropped replication slot pgsprite_b32f988b\n"+
		"  dropped publication pgsprite_b32f988b\n"+
		"  deleted the checkpoint row (phase catching_up)\n", text)
}

// A refusal says nothing was dropped and names its cause.
func TestRenderDiscardRefusalSaysNothingWasDropped(t *testing.T) {
	text := renderDiscard(discardReport{
		Table:   "public.orders",
		Outcome: discardOutcomeRefused,
		Cause:   discardCauseTableLockHeld,
		Detail:  "table lock is already held for public.orders by backend 4242",
	})
	assert.Equal(t, "refused to discard the run on public.orders (table-lock-held): "+
		"table lock is already held for public.orders by backend 4242\n"+
		"nothing was dropped\n", text)
}

// A discard that stopped partway names what it dropped before the error
// and says a second discard picks up what is left.
func TestRenderDiscardStoppedNamesWhatWasDropped(t *testing.T) {
	text := renderDiscard(discardReport{
		Table:   "public.orders",
		Outcome: discardOutcomeStopped,
		Cause:   discardCauseSlotActive,
		Detail:  "replication slot is held by another process",
		Shadow:  "_pgsprite_orders_new",
		schema:  "public",
	})
	assert.Equal(t, "the discard on public.orders stopped partway: replication slot is held by another process\n"+
		"  dropped shadow table public._pgsprite_orders_new\n"+
		"run discard public.orders again to drop what is left\n", text)
}

// A publication left after its slot is named, with what to do when a
// second discard leaves it again.
func TestRenderDiscardStoppedNamesAPublicationLeft(t *testing.T) {
	text := renderDiscard(discardReport{
		Table:           "public.orders",
		Outcome:         discardOutcomeStopped,
		Detail:          "no slot of the name remains; publication of its name left in place",
		PublicationLeft: "pgsprite_b32f988b",
	})
	assert.Equal(t, "the discard on public.orders stopped partway: no slot of the name remains; publication of its name left in place\n"+
		"  no replication slot pgsprite_b32f988b remains; its publication was left in place\n"+
		"run discard public.orders again to drop what is left\n"+
		"a publication pgsprite_b32f988b that discard leaves again is not of the engine's shape: drop it by hand once nothing uses it\n", text)
}

// A table with nothing to discard gets one line and no list.
func TestRenderDiscardNothing(t *testing.T) {
	text := renderDiscard(discardReport{Table: "public.orders", Outcome: discardOutcomeNothing})
	assert.Equal(t, "nothing to discard on public.orders: no checkpoint row, shadow table, write fence, or replication slot of a copy-and-swap run\n", text)
}

// Each error a discard can return is sorted into the cause its report
// names; an operational failure is not a refusal.
func TestDiscardRefusalSortsCauses(t *testing.T) {
	cause, ok := discardRefusal(fmt.Errorf("discard public.orders: %w", &dbconn.TableLockHeldError{Schema: "public", Table: "orders"}))
	require.True(t, ok)
	assert.Equal(t, discardCauseTableLockHeld, cause)

	cause, ok = discardRefusal(fmt.Errorf("discard public.orders: %w", dbconn.ErrSessionStartHidden))
	require.True(t, ok)
	assert.Equal(t, "table-lock-unconfirmed", cause, "the token migrate's verdict carries for the same refusal")

	cause, ok = discardRefusal(fmt.Errorf("discard unrecorded slot pgsprite_a725d037: %w", decode.ErrSlotActive))
	require.True(t, ok)
	assert.Equal(t, discardCauseSlotActive, cause)

	cause, ok = discardRefusal(fmt.Errorf("discard public.orders: %w", preflight.ErrCopySwapProofMismatch))
	require.True(t, ok)
	assert.Equal(t, string(verdict.ReasonInsufficientPrivileges), cause)

	_, ok = discardRefusal(errors.New("connection reset by peer"))
	assert.False(t, ok, "an operational failure is not a refusal")
}

// The outcome of a failed discard turns on whether anything is gone: a
// refusal before any drop is refused (exit 2), a refusal or a failure
// after a drop is stopped (exit 1) with the refusal's cause, and a failure
// before any drop that is not a refusal prints no report.
func TestDiscardFailureSortsOutcomes(t *testing.T) {
	none := schemachange.Discarded{Schema: "public", Table: "orders"}
	shadowGone := schemachange.Discarded{Schema: "public", Table: "orders", Shadow: "_pgsprite_orders_new"}
	held := fmt.Errorf("discard unrecorded slot pgsprite_a725d037: %w", decode.ErrSlotActive)
	reset := errors.New("connection reset by peer")

	outcome, cause := discardFailure(held, none)
	assert.Equal(t, discardOutcomeRefused, outcome)
	assert.Equal(t, discardCauseSlotActive, cause)

	outcome, cause = discardFailure(held, shadowGone)
	assert.Equal(t, discardOutcomeStopped, outcome, "the shadow is gone, so the refusal came partway")
	assert.Equal(t, discardCauseSlotActive, cause)

	outcome, cause = discardFailure(reset, shadowGone)
	assert.Equal(t, discardOutcomeStopped, outcome)
	assert.Empty(t, cause)

	outcome, cause = discardFailure(reset, none)
	assert.Empty(t, outcome, "a failure before any drop prints no report")
	assert.Empty(t, cause)

	outcome, _ = discardFailure(fmt.Errorf("discard unrecorded slot pgsprite_a725d037: %w", decode.ErrSlotDroppedPublicationLeft), none)
	assert.Equal(t, discardOutcomeStopped, outcome, "the slot is gone though the call names nothing it dropped")
}

// The discard report's cause row names each cause the command itself
// gives a refusal, so a script branching on cause can find every value.
func TestDocListsTheCausesDiscardNames(t *testing.T) {
	raw, err := os.ReadFile("../../docs/cli.md")
	require.NoError(t, err)
	_, section, found := strings.Cut(string(raw), "#### Discard report")
	require.True(t, found, "cli.md has the discard report section")
	_, row, found := strings.Cut(section, "\n| `cause` | ")
	require.True(t, found, "the discard report has a cause row")
	row, _, _ = strings.Cut(row, "\n")
	for _, cause := range []string{discardCauseTableLockHeld, discardCauseTableLockUnconfirmed, discardCauseSlotActive, string(verdict.ReasonInsufficientPrivileges)} {
		assert.Contains(t, row, "`"+cause+"`")
	}
}

// A bare table name is a usage error before any connection is opened.
func TestDiscardNeedsTheSchemaQualifiedTableName(t *testing.T) {
	cmd := &DiscardCmd{DBFlags: DBFlags{URL: "postgres://unused.invalid/db"}, Table: "orders"}
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.Error(t, err)
	assert.NotErrorIs(t, err, verdict.ErrRefused)
	assert.Empty(t, out.String())
}

// The discard report's documentation states the format version the
// command prints, in its prose and in its field table.
func TestDocStatesCurrentDiscardFormatVersion(t *testing.T) {
	raw, err := os.ReadFile("../../docs/cli.md")
	require.NoError(t, err)
	doc := string(raw)
	assert.Contains(t, doc, fmt.Sprintf("`discard --json` prints one object, `format_version` %d.", discardFormatVersion))
	assert.Contains(t, doc, fmt.Sprintf("| `format_version` | The report's shape, %d |", discardFormatVersion))
}
