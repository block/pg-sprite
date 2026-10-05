package schemachange

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Zero bounds take the defaults and a negative bound is refused: a negative
// attempt count would run the loop zero times and report exhaustion without
// ever trying, and a negative backoff would fire the timer at once.
func TestCutoverOptionsValidateRefusesNegativeBounds(t *testing.T) {
	require.NoError(t, CutoverOptions{}.validate())
	assert.Equal(t, defaultLockAttempts, CutoverOptions{}.lockAttempts())
	assert.Equal(t, defaultLockBackoff, CutoverOptions{}.lockBackoff())
	assert.Equal(t, 2, CutoverOptions{LockAttempts: 2}.lockAttempts())
	assert.Equal(t, time.Second, CutoverOptions{LockBackoff: time.Second}.lockBackoff())

	require.ErrorIs(t, CutoverOptions{LockAttempts: -1}.validate(), ErrInvalidOptions)
	require.ErrorIs(t, CutoverOptions{LockBackoff: -time.Millisecond}.validate(), ErrInvalidOptions)
	require.ErrorIs(t, CutoverOptions{Options: Options{LockTimeout: time.Microsecond}}.validate(), ErrInvalidOptions,
		"the embedded transaction bounds are validated too")
}

// Only the two SQLSTATEs a bounded lock acquisition is designed to end in
// are retried: lock_timeout expiry and a deadlock the server broke. A
// statement timeout, a refusal, or a plain error ends the loop, because
// retrying them would not change the answer.
func TestLockRetryableMatchesOnlyLockTimeoutAndDeadlock(t *testing.T) {
	assert.True(t, lockRetryable(fmt.Errorf("lock: %w", &pgconn.PgError{Code: codeLockNotAvailable})))
	assert.True(t, lockRetryable(&pgconn.PgError{Code: codeDeadlockDetected}))

	assert.False(t, lockRetryable(&pgconn.PgError{Code: "57014"}), "a statement timeout is not a lock wait")
	assert.False(t, lockRetryable(refuse(CauseSwapMismatch, nil, "catalog differs")))
	assert.False(t, lockRetryable(errors.New("not a server answer")))
	assert.False(t, lockRetryable(nil))
}

// A lost connection is recognised by the server's own termination codes,
// any connection-exception class code, a network error, an EOF, or pgx's
// report that nothing was sent; the answers the server did give — a lock
// timeout, a statement timeout — are not lost connections, so the outcome
// of those attempts is never inspected as if it were unknown.
func TestConnectionLostDistinguishesLostConnectionsFromServerAnswers(t *testing.T) {
	assert.True(t, connectionLost(&pgconn.PgError{Code: codeAdminShutdown}), "pg_terminate_backend")
	assert.True(t, connectionLost(&pgconn.PgError{Code: codeCrashShutdown}))
	assert.True(t, connectionLost(fmt.Errorf("commit: %w", &pgconn.PgError{Code: "08006"})), "connection_failure")
	assert.True(t, connectionLost(&net.OpError{Op: "write", Err: errors.New("broken pipe")}))
	assert.True(t, connectionLost(fmt.Errorf("read: %w", io.EOF)))
	assert.True(t, connectionLost(io.ErrUnexpectedEOF))

	assert.False(t, connectionLost(&pgconn.PgError{Code: codeLockNotAvailable}))
	assert.False(t, connectionLost(&pgconn.PgError{Code: "57014"}), "a statement timeout is an answer")
	assert.False(t, connectionLost(context.Canceled), "a cancelled context is the caller's decision, not a lost link")
	assert.False(t, connectionLost(nil))
}

// A caller's context ending mid-attempt leaves the outcome unknown just as
// a lost connection does: the COMMIT may have reached the server before
// the client stopped waiting for its answer. The server's own answers do
// not, so a lock or statement timeout is never inspected as unknown.
func TestOutcomeUnknownCoversLostConnectionsAndEndedContexts(t *testing.T) {
	assert.True(t, outcomeUnknown(fmt.Errorf("commit: %w", context.Canceled)))
	assert.True(t, outcomeUnknown(context.DeadlineExceeded))
	assert.True(t, outcomeUnknown(&pgconn.PgError{Code: codeAdminShutdown}), "a lost connection stays unknown")

	assert.False(t, outcomeUnknown(&pgconn.PgError{Code: codeLockNotAvailable}), "a lock timeout is the server's answer")
	assert.False(t, outcomeUnknown(&pgconn.PgError{Code: "57014"}), "a statement timeout is the server's answer")
	assert.False(t, outcomeUnknown(errors.New("plain failure")))
	assert.False(t, outcomeUnknown(nil))
}

// The identity clause replays every declared option of the source
// sequence and nothing about its position, so the recreated sequence's
// catalog row equals the source's and the position is set separately.
func TestIdentityClauseReplaysTheDeclaredOptions(t *testing.T) {
	always := IdentityColumn{Column: "id", Always: true, Options: SequenceOptions{
		Start: 10, Increment: 2, Min: 1, Max: 9223372036854775807, Cache: 1, Cycle: false,
	}}
	assert.Equal(t,
		"GENERATED ALWAYS AS IDENTITY (INCREMENT BY 2 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 10 CACHE 1 NO CYCLE)",
		identityClause(always))

	byDefault := IdentityColumn{Column: "seqno", Always: false, Options: SequenceOptions{
		Start: -1, Increment: -1, Min: -100, Max: -1, Cache: 20, Cycle: true,
	}}
	assert.Equal(t,
		"GENERATED BY DEFAULT AS IDENTITY (INCREMENT BY -1 MINVALUE -100 MAXVALUE -1 START WITH -1 CACHE 20 CYCLE)",
		identityClause(byDefault))
}

// The default sleep ends early with the context's error, so a cancelled
// cutover does not sit out a backoff before noticing; a zero wait returns
// at once.
func TestSleepContextReturnsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, sleepContext(ctx, time.Hour), context.Canceled)
	require.NoError(t, sleepContext(t.Context(), 0))
}
