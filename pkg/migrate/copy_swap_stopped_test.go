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
	assert.Contains(t, stoppedRunDetail("shop.orders", stoppedRetype, true), "--restart SCHEMA.TABLE")
	assert.NotContains(t, stoppedRunDetail("shop.orders", stoppedRetype, false), "--restart")
}
