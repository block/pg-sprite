package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// Each acknowledgement's wrong-table error carries its own sentinel and no
// other acknowledgement's, so an embedder can tell which input to send
// back, and its message still names the resolved table and the one given.
// The accept-blocking check reads the table's lock first, so it is not a
// pure function and is pinned by the irreversible-operations registry's
// integration test; its sentinel is still one the others must not match.
func TestEachMismatchHasItsOwnSentinel(t *testing.T) {
	st, err := statement.ParseOne(`ALTER TABLE app.orders DROP COLUMN note`)
	require.NoError(t, err)

	checks := map[string]struct {
		err  error
		want error
		msg  string
	}{
		"force": {
			err:  checkForceAck(st, "app.other"),
			want: ErrForceMismatch,
			msg:  `the force acknowledgement must name the resolved target table "app.orders", got "app.other"; nothing was executed`,
		},
		"restart": {
			err:  checkRestartAck(st, "app.other"),
			want: ErrRestartMismatch,
			msg:  `the restart acknowledgement must name the resolved target table "app.orders", got "app.other"; nothing was executed`,
		},
		"block-writes": {
			err:  checkBlockWritesAck(st, "app.other"),
			want: ErrBlockWritesMismatch,
			msg:  `the block-writes acknowledgement must name the resolved target table "app.orders", got "app.other"; nothing was executed`,
		},
		"allow-destructive": {
			err:  checkAllowDestructiveAck("app.orders", "app.other"),
			want: ErrAllowDestructiveMismatch,
			msg:  `the destructive acknowledgement must name the resolved target table "app.orders", got "app.other"; nothing was executed`,
		},
	}
	all := map[string]error{
		"force":             ErrForceMismatch,
		"restart":           ErrRestartMismatch,
		"block-writes":      ErrBlockWritesMismatch,
		"allow-destructive": ErrAllowDestructiveMismatch,
		"accept-blocking":   ErrAcceptBlockingMismatch,
	}
	for name, c := range checks {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, c.err, c.want)
			assert.EqualError(t, c.err, c.msg)
			for otherName, other := range all {
				if otherName == name {
					continue
				}
				assert.NotErrorIs(t, c.err, other, "a %s mismatch must not answer for %s", name, otherName)
			}
		})
	}
}
