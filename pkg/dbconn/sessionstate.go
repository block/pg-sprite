package dbconn

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSessionSettingsDiscarded reports a connection that did not keep the
// session settings the pool asked for. The engine's execution bounds are
// session settings, so a connection that drops them removes the bounds
// without removing the work they were bounding.
var ErrSessionSettingsDiscarded = errors.New("the connection does not keep pg-sprite's session settings")

// ErrNoSessionAffinity reports a connection that does not keep one server
// session, so nothing set on it — least of all an execution bound — can be
// relied on to still be there for the next statement.
var ErrNoSessionAffinity = errors.New("the connection does not keep one PostgreSQL session")

// sessionEndpointRemedy names what an operator does about a connection that
// cannot hold session state. Transaction pooling is the cause in practice,
// statement pooling the stricter variant, and every hosted platform that
// defaults to either also offers a session-mode endpoint — usually the same
// host on a different port.
const sessionEndpointRemedy = "connect directly or through a session-mode endpoint " +
	"(on hosted PostgreSQL this is usually the same host on a different port), or run the pooler in session pool mode; " +
	"transaction and statement pooling are not supported"

// sessionBound is one execution bound the pool asks a server session to
// hold, in the milliseconds PostgreSQL reads a bare integer as.
type sessionBound struct {
	name string
	// requested is the duration the caller asked for; ms is that duration as
	// the server counts it, which truncates anything under a millisecond to 0.
	requested time.Duration
	ms        int64
}

// applied renders the bound the pool sent the server: the whole milliseconds
// it set, or, for a request that truncated to 0, the duration asked for, since
// "0" would read as the very unbounded value the check refuses.
func (b sessionBound) applied() string {
	if b.ms == 0 {
		return b.requested.String()
	}
	return describeTimeout(b.ms)
}

// sessionBounds lists the timeouts the pool applies and then relies on for
// every statement the engine issues. Applying and verifying read the same
// list, so a bound can never be set without also being checked, and the
// order is fixed so a failure names the same setting every time.
func sessionBounds(lock, statement time.Duration) []sessionBound {
	return []sessionBound{
		{name: "lock_timeout", requested: lock, ms: lock.Milliseconds()},
		{name: "statement_timeout", requested: statement, ms: statement.Milliseconds()},
	}
}

// verifySessionSettings reads back the timeouts the pool asked for and fails
// when the server session does not carry them.
//
// The check reads the server rather than the connection string because the
// failure it catches is invisible from the client side. A transaction-mode
// pooler either rejects an unknown startup parameter outright — a loud
// failure that needs no help — or is configured to ignore it, in which case
// the connection succeeds and the setting is simply absent. Only the value
// the server actually holds distinguishes the second case from success.
//
// Each value is read with SHOW, which reads the session's own setting without
// looking anything up in the catalog. A query that names a relation or a
// function can wait on a catalog lock, which a vacuum truncating a catalog or
// another session's DDL holds briefly, and that wait runs under the very
// lock_timeout being verified: on a busy server, a session bounded at 1ms
// could not read its own bound back.
//
// INV: LK-2 — every strong lock acquisition is bounded. lock_timeout is how
// that bound is applied, so a session without it is unbounded: an ALTER that
// queues sits at the head of the lock queue indefinitely, blocking every
// reader and writer of the table behind it.
func verifySessionSettings(ctx context.Context, pool *pgxpool.Pool, want []sessionBound) error {
	for _, bound := range want {
		// SHOW takes no placeholder, so the name is interpolated — it is
		// one of this package's own bounds, never caller-supplied text.
		var shown string
		if err := pool.QueryRow(ctx, "SHOW "+bound.name).Scan(&shown); err != nil {
			return fmt.Errorf("read session %s: %w", bound.name, err)
		}
		got, err := shownMilliseconds(shown)
		if err != nil {
			return fmt.Errorf("read session %s: %w", bound.name, err)
		}
		// The server holding 0 is never a bound, even when 0 is what the
		// requested duration truncated to: a sub-millisecond request would
		// otherwise read back as exactly what was sent.
		if got != bound.ms || got == 0 {
			return fmt.Errorf("%w: %s is %s, not the %s this pool set; %s",
				ErrSessionSettingsDiscarded, bound.name,
				describeTimeout(got), bound.applied(), sessionEndpointRemedy)
		}
	}
	return nil
}

// shownUnitMilliseconds is the number of milliseconds in each unit SHOW
// renders a millisecond setting in. PostgreSQL names the largest unit that
// divides the value exactly, so 1500 shows as "1500ms" and 3000 as "3s", and
// it prints 0 with no unit.
func shownUnitMilliseconds(unit string) (int64, bool) {
	switch unit {
	case "", "ms":
		return 1, true
	case "s":
		return 1000, true
	case "min":
		return 60 * 1000, true
	case "h":
		return 60 * 60 * 1000, true
	case "d":
		return 24 * 60 * 60 * 1000, true
	default:
		return 0, false
	}
}

// shownMilliseconds converts a timeout as SHOW renders it back into
// milliseconds. Any other text is an error rather than a guess, so a
// rendering this package does not know refuses the pool instead of being
// read as some bound.
func shownMilliseconds(shown string) (int64, error) {
	digits := strings.TrimRightFunc(shown, unicode.IsLetter)
	unit, known := shownUnitMilliseconds(shown[len(digits):])
	if !known {
		return 0, fmt.Errorf("SHOW returned %q, whose unit is not one PostgreSQL shows a timeout in", shown)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("SHOW returned %q, which is not a whole number of a unit: %w", shown, err)
	}
	if n < 0 || n > math.MaxInt64/unit {
		return 0, fmt.Errorf("SHOW returned %q, which is not a timeout PostgreSQL can hold", shown)
	}
	return n * unit, nil
}

// describeTimeout renders a millisecond timeout for an operator, naming zero
// as what it means rather than printing it as a number: a reader who does
// not know PostgreSQL's convention cannot tell that 0 is the dangerous value.
func describeTimeout(ms int64) string {
	if ms == 0 {
		return "unbounded"
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

// applySessionBounds returns an AfterConnect hook that sets the execution
// bounds as statements on every new connection, so they survive a pooler
// that does not forward startup parameters.
func applySessionBounds(lock, statement time.Duration) func(context.Context, *pgx.Conn) error {
	return func(ctx context.Context, conn *pgx.Conn) error {
		// One statement per Exec, so a failure names the bound that was
		// not applied rather than the pair. A zero-argument Exec goes out
		// on the simple protocol, which would carry both in one string;
		// the split is for the error, not for the protocol.
		//
		// SET takes no placeholder, so the value is interpolated — it is a
		// duration resolved by this package, never caller-supplied text.
		for _, bound := range sessionBounds(lock, statement) {
			if _, err := conn.Exec(ctx, fmt.Sprintf("SET %s = %d", bound.name, bound.ms)); err != nil {
				return fmt.Errorf("apply session %s: %w", bound.name, err)
			}
		}
		return nil
	}
}
