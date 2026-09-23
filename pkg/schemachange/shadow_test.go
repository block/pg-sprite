package schemachange

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// The zero proof and a proof with any identifying field blank are refused
// before a database round trip: a forged CopySwapTarget cannot carry the
// builder past ST-6.
func TestCheckProofRefusesTheZeroProof(t *testing.T) {
	require.ErrorIs(t, checkProof(preflight.CopySwapTarget{}), ErrInvariantViolation)
	require.Equal(t, CauseProofEmpty, RefusalCauseOf(checkProof(preflight.CopySwapTarget{})))
}

// PostgreSQL counts timeouts in whole milliseconds and reads zero as
// disabled: a nonzero value that rounds to zero, or a negative one, would
// silently remove the bound, so it is refused; zero keeps the default and
// one millisecond is the smallest value that encodes.
func TestOptionsValidateRefusesTimeoutsBelowOneMillisecond(t *testing.T) {
	require.NoError(t, Options{}.validate())
	require.NoError(t, Options{LockTimeout: time.Millisecond, StatementTimeout: time.Millisecond}.validate())
	require.ErrorIs(t, Options{LockTimeout: time.Microsecond}.validate(), ErrInvalidOptions)
	require.ErrorIs(t, Options{StatementTimeout: 999 * time.Microsecond}.validate(), ErrInvalidOptions)
	require.ErrorIs(t, Options{StatementTimeout: -time.Second}.validate(), ErrInvalidOptions)
}

// The ST-7 re-proof holds the retargeted text to two facts independently of
// the retarget that produced it: it must name the shadow and nothing else,
// and it must carry the gated statement's operations unchanged.
func TestProveRetargetRefusesTextThatIsNotTheGatedStatementOnTheShadow(t *testing.T) {
	gated, err := statement.ParseOne(`ALTER TABLE sales.widgets ADD COLUMN note text`)
	require.NoError(t, err)
	shadow := ShadowName("sales", "widgets")

	require.NoError(t, proveRetarget(gated, "sales", shadow, `ALTER TABLE sales.`+shadow+` ADD COLUMN note text`))

	err = proveRetarget(gated, "sales", shadow, `ALTER TABLE sales.other ADD COLUMN note text`)
	require.ErrorIs(t, err, ErrInvariantViolation, "a retarget onto another relation")
	require.Equal(t, CauseStatementTarget, RefusalCauseOf(err))

	err = proveRetarget(gated, "sales", shadow, `ALTER TABLE sales.`+shadow+` ADD COLUMN note text NOT NULL`)
	require.ErrorIs(t, err, ErrInvariantViolation, "an operation that differs beyond the target")
	require.Equal(t, CauseStatementTarget, RefusalCauseOf(err))

	err = proveRetarget(gated, "sales", shadow, `ALTER TABLE sales.`+shadow+` ADD COLUMN`)
	require.ErrorIs(t, err, ErrInvariantViolation, "text that does not re-parse")
	require.Equal(t, CauseStatementTarget, RefusalCauseOf(err))
}
