package dbconn

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// and every hosted platform that defaults to it also offers a session-mode
// endpoint — usually the same host on a different port.
const sessionEndpointRemedy = "connect through a session-mode endpoint instead of a transaction-mode pooler " +
	"(on hosted PostgreSQL this is usually the same host on a different port), or run the pooler in session pool mode"

// sessionBound is one execution bound the pool asks a server session to
// hold, in the milliseconds PostgreSQL reads a bare integer as.
type sessionBound struct {
	name string
	ms   int64
}

// sessionBounds lists the timeouts the pool applies and then relies on for
// every statement the engine issues. Applying and verifying read the same
// list, so a bound can never be set without also being checked, and the
// order is fixed so a failure names the same setting every time.
func sessionBounds(lock, statement time.Duration) []sessionBound {
	return []sessionBound{
		{name: "lock_timeout", ms: lock.Milliseconds()},
		{name: "statement_timeout", ms: statement.Milliseconds()},
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
// Values are compared in milliseconds via pg_settings rather than against
// current_setting's text, which renders 3000ms as "3s" and 500ms as "500ms"
// — reproducing those unit rules here would be a second copy of them.
//
// INV: LK-2 — every strong lock acquisition is bounded. lock_timeout is how
// that bound is applied, so a session without it is unbounded: an ALTER that
// queues sits at the head of the lock queue indefinitely, blocking every
// reader and writer of the table behind it.
func verifySessionSettings(ctx context.Context, pool *pgxpool.Pool, want []sessionBound) error {
	for _, bound := range want {
		var got int64
		err := pool.QueryRow(ctx,
			"SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = $1", bound.name).Scan(&got)
		if err != nil {
			return fmt.Errorf("read session %s: %w", bound.name, err)
		}
		if got != bound.ms {
			return fmt.Errorf("%w: %s is %s, not the %s this pool set; %s",
				ErrSessionSettingsDiscarded, bound.name,
				describeTimeout(got), describeTimeout(bound.ms), sessionEndpointRemedy)
		}
	}
	return nil
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
		// One statement per Exec: the extended protocol rejects a query
		// string carrying several commands, and the pool's exec mode is a
		// caller option.
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
