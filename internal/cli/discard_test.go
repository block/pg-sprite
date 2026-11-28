package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
