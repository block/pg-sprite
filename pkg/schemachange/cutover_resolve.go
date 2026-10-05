package schemachange

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLSTATE codes that mean the connection ended without an answer to the
// statement in flight: the server terminated this backend, crashed, or
// reported a connection exception. These leave the swap's outcome unknown
// until the catalog is inspected (LK-4).
const (
	codeAdminShutdown        = "57P01"
	codeCrashShutdown        = "57P02"
	classConnectionException = "08"
)

// rolledBack marks a failure whose attempt is known to have left the
// source live.
func rolledBack(err error) error {
	return fmt.Errorf("%w: %w", ErrCutoverRolledBack, err)
}

// resolveUnknownOutcome learns what became of an attempt the client stopped
// hearing from (LK-4). The attempt's backend may still be deciding — inside
// COMMIT behind a synchronous standby or a deferred trigger — so it is
// first followed to its exit, under a context the caller's cancellation no
// longer governs: the answer is owed whatever cut the attempt short. Then
// a fresh connection reads which relation bears the source name: the
// shadow means the swap committed and is the success it was, with the
// identities the handoff left on the live table read back from it; the
// source means the attempt rolled back; a name borne by neither relation,
// or a backend that outlives its own timeouts, is refused.
func resolveUnknownOutcome(ctx context.Context, pool *pgxpool.Pool, ready CutoverReady, backend swapBackend, attempt int, lost error, opts CutoverOptions) (SwappedTable, error) {
	ctx = context.WithoutCancel(ctx)
	schema, source := ready.built.Schema(), ready.built.SourceTable()
	if backend.pid == 0 {
		// The attempt ended before its transaction ran a statement, so
		// there is nothing on the server to follow or to read.
		return SwappedTable{}, rolledBack(fmt.Errorf("cutover of %s.%s ended before its transaction began: %w", schema, source, lost))
	}
	if err := awaitBackendExit(ctx, pool, backend, opts.lockTimeout()+opts.statementTimeout(), opts.sleep()); err != nil {
		// INV: LK-4
		return SwappedTable{}, refuse(CauseOutcomeAmbiguous, []error{lost, err},
			"the outcome of the cutover of %s.%s cannot be read while its attempt is still running", schema, source)
	}
	outcome, err := inspectOutcome(ctx, pool, ready.built)
	if err != nil {
		return SwappedTable{}, errors.Join(lost, err)
	}
	switch outcome {
	case outcomeSwapped:
		identities, err := readLiveIdentities(ctx, pool, ready.built)
		if err != nil {
			return SwappedTable{}, errors.Join(lost, err)
		}
		// INV: LK-4
		return newSwappedTable(ready, identities, attempt), nil
	case outcomeNotSwapped:
		return SwappedTable{}, rolledBack(fmt.Errorf("cutover of %s.%s rolled back when its attempt was cut short: %w", schema, source, lost))
	default:
		// INV: LK-4
		return SwappedTable{}, refuse(CauseOutcomeAmbiguous, []error{lost},
			"after a lost attempt neither the source nor the shadow bears the name %s.%s", schema, source)
	}
}

// outcomeUnknown reports whether a failed attempt ended without an answer
// from the server: the connection was lost, or the context ended while a
// statement was in flight. pgx answers an ended context by cancelling the
// statement, but the cancel request travels separately from the statement
// and may never arrive; either way the server finishes what it was doing
// on its own, so the outcome has to be read, not inferred.
func outcomeUnknown(err error) bool {
	return connectionLost(err) || contextEnded(err)
}

// contextEnded reports whether the failure is the caller's context being
// cancelled or timing out.
func contextEnded(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// connectionLost reports whether a failed attempt lost its connection
// rather than receiving an answer: the server terminated the backend, a
// connection-exception SQLSTATE came back, the socket failed, or pgx
// reports the connection closed. Only then is the outcome unknown.
func connectionLost(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == codeAdminShutdown || pgErr.Code == codeCrashShutdown || strings.HasPrefix(pgErr.Code, classConnectionException)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || pgconn.SafeToRetry(err)
}
