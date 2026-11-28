package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/checkpoint"
)

// stoppedRetype is a copy-and-swap run of a retype stopped in verifying,
// before its swap, where a restart could still discard it.
var stoppedRetype = stoppedRun{
	cp: checkpoint.Checkpoint{
		Statement: `ALTER TABLE shop.orders ALTER COLUMN qty TYPE bigint`,
		Phase:     checkpoint.PhaseVerifying,
	},
	found: true,
}

// The declarative door's remedy is a command that runs: resuming the
// stopped statement needs the copy-and-swap route opened, and without
// --copy-and-swap the resume is refused copy-and-swap-not-enabled.
func TestDesiredRunStoppedDetailNamesTheFlagThatOpensTheRoute(t *testing.T) {
	detail := desiredRunStoppedDetail("the live table matches the desired schema", "shop", "orders", stoppedRetype)
	assert.Contains(t, detail, "migrate --alter and --copy-and-swap")
}

// The restart is offered only to a statement that can route to
// copy-and-swap; an index rename or drop never can, so its refusal names
// only finishing the stopped run.
func TestStoppedRunDetailOffersTheRestartOnlyToARestartableStatement(t *testing.T) {
	assert.Contains(t, stoppedRunDetail("shop.orders", stoppedRetype, true), "run it with --copy-and-swap --restart shop.orders")
	assert.NotContains(t, stoppedRunDetail("shop.orders", stoppedRetype, false), "--restart")
}

// The detail is the CLI operator's: it names the CLI's flags and the
// discard command for the stopped table, and none of the library's option
// names, which a library host has no use for in prose and a CLI operator
// cannot type.
func TestStoppedRunDetailNamesOnlyCLIFlagsAndCommands(t *testing.T) {
	for _, restartable := range []bool{true, false} {
		detail := stoppedRunDetail("shop.orders", stoppedRetype, restartable)
		assert.Contains(t, detail, "pg-sprite discard shop.orders", "restartable=%v", restartable)
		assert.NotContains(t, detail, "CopySwapOptions", "restartable=%v", restartable)
		assert.NotContains(t, detail, "Enabled", "restartable=%v", restartable)
	}
}

// A run stopped in cutover may have committed its swap, which neither a
// restart nor a discard undoes, so only finishing it is offered.
func TestStoppedRunDetailOffersOnlyFinishingAStoppedCutover(t *testing.T) {
	cutover := stoppedRetype
	cutover.cp.Phase = checkpoint.PhaseCutover
	detail := stoppedRunDetail("shop.orders", cutover, true)
	assert.Contains(t, detail, "run the stopped statement again to finish it, then run this one")
	assert.NotContains(t, detail, "--restart")
	assert.NotContains(t, detail, "pg-sprite discard")
}
