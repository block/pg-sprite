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

// Every wrong-table check returns one typed error carrying the
// acknowledgement's name, the table it must name, and the one given, so an
// adapter reads both tables from fields with one errors.As instead of
// parsing the message or asking errors.Is of every sentinel. The type still
// answers errors.Is for its own sentinel and no other.
func TestEachMismatchIsAnAcknowledgementMismatchError(t *testing.T) {
	st, err := statement.ParseOne(`ALTER TABLE app.orders DROP COLUMN note`)
	require.NoError(t, err)

	checks := map[string]struct {
		err  error
		want error
	}{
		"force":             {err: checkForceAck(st, "app.other"), want: ErrForceMismatch},
		"restart":           {err: checkRestartAck(st, "app.other"), want: ErrRestartMismatch},
		"block-writes":      {err: checkBlockWritesAck(st, "app.other"), want: ErrBlockWritesMismatch},
		"allow-destructive": {err: checkAllowDestructiveAck("app.orders", "app.other"), want: ErrAllowDestructiveMismatch},
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
			var mismatch *AcknowledgementMismatchError
			require.ErrorAs(t, c.err, &mismatch)
			assert.Equal(t, name, mismatch.Acknowledgement)
			assert.Contains(t, Acknowledgements(), mismatch.Acknowledgement)
			assert.Equal(t, "app.orders", mismatch.Table)
			assert.Equal(t, "app.other", mismatch.Given)
			assert.ErrorIs(t, mismatch, c.want)
			for otherName, other := range all {
				if otherName == name {
					continue
				}
				assert.NotErrorIs(t, mismatch, other, "a %s mismatch must not answer for %s", name, otherName)
			}
		})
	}
}

// The accept-blocking mismatch is built after the statement's lock is read,
// so it is pinned here directly: its message names the statement kind
// whose lock was acknowledged, and it answers only its own sentinel.
func TestAcceptBlockingMismatchNamesTheLockingStatement(t *testing.T) {
	mismatch := ackAcceptBlocking.mismatch("app.orders", "app.other")
	mismatch.locks = statement.KindDropIndex
	var err error = mismatch

	assert.EqualError(t, err,
		`the accept-blocking acknowledgement must name the table the statement locks: DROP INDEX locks "app.orders", got "app.other"; nothing was executed`)
	assert.ErrorIs(t, err, ErrAcceptBlockingMismatch)
	for _, other := range []error{ErrForceMismatch, ErrRestartMismatch, ErrBlockWritesMismatch, ErrAllowDestructiveMismatch} {
		assert.NotErrorIs(t, err, other)
	}
}
