package schemachange

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ErrLockRetriesExhausted reports that every bounded attempt to take ACCESS
// EXCLUSIVE on the source timed out behind other lock holders (LK-2). The
// swap executed nothing; the error wraps the server's last lock_timeout
// refusal, so errors.As against *pgconn.PgError still reaches it.
var ErrLockRetriesExhausted = errors.New("cutover lock retries exhausted")

// SQLSTATE codes a bounded ACCESS EXCLUSIVE acquisition retries on: the
// lock_timeout expiry every attempt is designed to end in, and a deadlock
// the server resolved by cancelling this side.
const (
	codeLockNotAvailable = "55P03"
	codeDeadlockDetected = "40P01"
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

// Default bounds for the ACCESS EXCLUSIVE acquisition: five tries with a
// half-second step keeps the whole retry window under the default
// statement_timeout while giving a busy table several chances.
const (
	defaultLockAttempts = 5
	defaultLockBackoff  = 500 * time.Millisecond
)

// DrainFunc flushes every captured change through the final WAL position
// into the shadow, inside the swap transaction and after ACCESS EXCLUSIVE
// on the source has excluded every writer, so no row can change after the
// drain and before the rename. The transaction is bounded by the swap's
// options and runs as the table's owner. A nil DrainFunc drains nothing: a
// quiesced table, or a caller that has already applied the final position.
type DrainFunc func(ctx context.Context, tx pgx.Tx) error

// CutoverOptions bounds the swap transaction and the ACCESS EXCLUSIVE
// acquisition that opens it. Zero values take the defaults; Options bounds
// every lock wait and statement inside the transaction as it does for the
// other shadow operations.
type CutoverOptions struct {
	Options
	// LockAttempts is how many times the swap tries to take ACCESS
	// EXCLUSIVE before giving up (LK-2). Zero means the default.
	LockAttempts int
	// LockBackoff is the wait after the first failed attempt; each later
	// wait is one step longer. Zero means the default.
	LockBackoff time.Duration
	// Sleep waits between attempts; nil waits on the wall clock. Tests
	// inject one to drive the retry loop without real time passing.
	Sleep func(ctx context.Context, d time.Duration) error
}

// validate refuses bounds the retry loop could not honour.
func (o CutoverOptions) validate() error {
	if err := o.Options.validate(); err != nil {
		return err
	}
	// INV: LK-2
	if o.LockAttempts < 0 {
		return fmt.Errorf("%w: lock attempts %d is negative; use zero for the default", ErrInvalidOptions, o.LockAttempts)
	}
	if o.LockBackoff < 0 {
		return fmt.Errorf("%w: lock backoff %s is negative; use zero for the default", ErrInvalidOptions, o.LockBackoff)
	}
	return nil
}

func (o CutoverOptions) lockAttempts() int {
	if o.LockAttempts == 0 {
		return defaultLockAttempts
	}
	return o.LockAttempts
}

func (o CutoverOptions) lockBackoff() time.Duration {
	if o.LockBackoff == 0 {
		return defaultLockBackoff
	}
	return o.LockBackoff
}

func (o CutoverOptions) sleep() func(context.Context, time.Duration) error {
	if o.Sleep == nil {
		return sleepContext
	}
	return o.Sleep
}

// sleepContext waits for d or until ctx ends, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Cutover swaps the verified shadow in for the source, in one transaction
// under ACCESS EXCLUSIVE on both tables, and returns the SwappedTable proof
// of what the live catalog now holds. The transaction takes the lock under
// lock_timeout and, when the wait times out or the server breaks a
// deadlock, rolls back and tries again after a bounded backoff — the
// engine never queues behind readers (LK-2). Under the lock it runs the
// caller's drain, re-runs the ST-5 checklist so the pairing it renames by
// is as fresh as the lock, renames the source and its dependents to their
// derived _old names and the shadow and its paired dependents to the names
// the source held (D8), hands every sequence and identity to the live table
// (D5), re-reads the catalog to confirm each rename and handoff took, and
// commits. The old table is left in place for DropOldTable (D9).
//
// A failed attempt is never assumed to have rolled back: before retrying
// or returning, the catalog is read from a fresh connection to learn which
// relation now bears the source name (LK-4). A commit the connection
// dropped on is reported as the success it was; a name neither the source
// nor the shadow bears is refused rather than guessed at.
//
// The caller holds the per-table lock (LK-1) and passes the CutoverReady
// the gate minted; a zero proof is refused before any connection opens.
func Cutover(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, ready CutoverReady, drain DrainFunc, opts CutoverOptions) (SwappedTable, error) {
	if err := opts.validate(); err != nil {
		return SwappedTable{}, err
	}
	if err := checkCutoverProofs(ready.built, ready.verified); err != nil {
		return SwappedTable{}, err
	}
	if err := requireTableLock(lock, ready.built.Schema(), ready.built.SourceTable()); err != nil {
		return SwappedTable{}, err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	swapped, err := cutoverWithRetry(ctx, pool, lock, ready, drain, opts)
	if err != nil {
		return SwappedTable{}, lockLossCause(lock, err)
	}
	return swapped, nil
}

// cutoverWithRetry is the LK-2 loop around one swap attempt. A lock
// timeout or a deadlock is the server telling this side it rolled back,
// so the loop waits out the backoff and tries again. A lost connection is
// the one failure that says nothing about the outcome, so the catalog is
// asked (LK-4) before the error is returned. Every other failure is a
// refusal or a statement error the server answered, which means the
// transaction rolled back, and the loop returns it as is.
func cutoverWithRetry(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, ready CutoverReady, drain DrainFunc, opts CutoverOptions) (SwappedTable, error) {
	attempts := opts.lockAttempts()
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		swapped, err := swapOnce(ctx, pool, lock, ready, drain, opts.Options)
		if err == nil {
			swapped.attempts = attempt
			return swapped, nil
		}
		if connectionLost(err) {
			return resolveLostConnection(ctx, pool, ready, attempt, err)
		}
		if !lockRetryable(err) {
			return SwappedTable{}, err
		}
		last = err
		if attempt == attempts {
			break
		}
		if err := opts.sleep()(ctx, opts.lockBackoff()*time.Duration(attempt)); err != nil {
			return SwappedTable{}, err
		}
	}
	// INV: LK-2
	return SwappedTable{}, fmt.Errorf("%w after %d attempts on %s.%s: %w", ErrLockRetriesExhausted, attempts, ready.built.Schema(), ready.built.SourceTable(), last)
}

// resolveLostConnection reads the outcome of an attempt whose connection
// broke from a fresh connection (LK-4): a swap the server committed before
// the client lost it is the success it was; a source still under its own
// name rolled back, and the connection error is returned for the caller to
// decide on; a name borne by neither relation is refused.
func resolveLostConnection(ctx context.Context, pool *pgxpool.Pool, ready CutoverReady, attempt int, lost error) (SwappedTable, error) {
	outcome, err := inspectOutcome(ctx, pool, ready.built)
	if err != nil {
		return SwappedTable{}, errors.Join(lost, err)
	}
	schema, source := ready.built.Schema(), ready.built.SourceTable()
	switch outcome {
	case outcomeSwapped:
		// INV: LK-4
		return newSwappedTable(ready, attempt), nil
	case outcomeNotSwapped:
		return SwappedTable{}, fmt.Errorf("cutover of %s.%s rolled back when its connection was lost: %w", schema, source, lost)
	default:
		// INV: LK-4
		return SwappedTable{}, refuse(CauseOutcomeAmbiguous, []error{lost},
			"after a lost connection neither the source nor the shadow bears the name %s.%s", schema, source)
	}
}

// lockRetryable reports whether a failed attempt ended the way a bounded
// lock acquisition is designed to end: the lock wait timed out, or the
// server broke a deadlock by cancelling this side. Both are answers from
// the server, so the transaction is known to have rolled back.
func lockRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == codeLockNotAvailable || pgErr.Code == codeDeadlockDetected
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
