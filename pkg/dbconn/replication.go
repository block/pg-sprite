package dbconn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

// ReplicationOption adjusts how ConnectReplication opens its connection.
type ReplicationOption func(*pgconn.Config)

// WithServerSideCancel makes a command whose context ends be cancelled on
// the server before the client gives it up: a cancel request goes to the
// walsender on a second connection, and the command's call returns once
// the server has answered it, or after grace with the connection cut.
// Without the option the client cuts the connection first and sends the
// cancel request afterwards, in the background on a budget of its own, so
// the call returns before the server has acted on it: a caller that reads
// the catalog next, or releases a holder, can find the command it was told
// ended still waiting or completing on the server — PostgreSQL does not
// bound a replication command by statement_timeout, and a walsender in a
// DROP_REPLICATION_SLOT … WAIT or a CREATE_REPLICATION_SLOT comes off its
// wait only for the cancel or for the holder. With the option, when the
// call returns the server has ended the command and reported it as
// query_canceled, which the error carries; the caller folds its context's
// error back in. Not for a streaming connection: its reads are bounded by
// per-read contexts that end routinely and must not end the stream.
func WithServerSideCancel(grace time.Duration) ReplicationOption {
	return func(c *pgconn.Config) {
		c.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
			return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: grace}
		}
	}
}

// ConnectReplication opens one dedicated connection in logical replication
// mode (replication=database) on cfg's database, for the replication
// commands that no ordinary connection accepts: CREATE_REPLICATION_SLOT,
// DROP_REPLICATION_SLOT, START_REPLICATION. It dials exactly as a pooled
// connection would — the same TLS posture, connect timeout, startup
// parameters including application_name, and BeforeConnect hook for
// short-lived credentials — so a caller never assembles a DSN of its own.
// The one session setting it owns is client_min_messages: once connected it
// runs SET client_min_messages = warning itself, so a walsender's warning
// that it will withhold changes reaches the connection whatever the role,
// the database, the server, a login event trigger, or the URL says. A
// client_min_messages in the URL is not honoured; a startup parameter would
// be, but a login trigger runs after startup parameters and can outrank
// one, a URL can carry the key in another case and pgx keeps both, and a
// connection pooler may refuse it or drop it on the floor.
//
// The connection is a bare pgconn, not a pgx.Conn: a walsender accepts only
// the simple query protocol, so the session preparation a pooled connection
// receives (the lock_timeout and statement_timeout SET statements and the
// search_path repair) is not run on it. Neither matters here: a replication
// connection issues replication commands, which PostgreSQL does not bound by
// statement_timeout at all, and reads no catalog by name. The caller bounds
// every command with ctx and closes the connection when it is done; a
// command's context ending cuts the connection on the client's side only,
// unless the caller asked for WithServerSideCancel.
func ConnectReplication(ctx context.Context, cfg Config, opts ...ReplicationOption) (*pgconn.PgConn, error) {
	pc, err := buildPoolConfig(cfg)
	if err != nil {
		return nil, err
	}
	connConfig := pc.ConnConfig.Copy()
	if cfg.BeforeConnect != nil {
		if err := cfg.BeforeConnect(ctx, connConfig); err != nil {
			return nil, fmt.Errorf("before connect for replication: %w", err)
		}
	}
	connConfig.RuntimeParams["replication"] = "database"
	for _, opt := range opts {
		opt(&connConfig.Config)
	}
	conn, err := pgconn.ConnectConfig(ctx, &connConfig.Config)
	if err != nil {
		return nil, fmt.Errorf("connect replication session: %w", err)
	}
	// A walsender reports what it will withhold from the stream as a
	// warning. SET on the open session is the last word on
	// client_min_messages: it lands after the role's, the database's, and
	// the server's settings and after any login event trigger, none of
	// which can then keep that warning from the connection. A walsender
	// takes the simple query protocol, which is what Exec speaks.
	if _, err := conn.Exec(ctx, "SET client_min_messages = warning").ReadAll(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("ask the replication session for warnings: %w", err),
			conn.Close(ctx))
	}
	return conn, nil
}
