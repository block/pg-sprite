package dbconn

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

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
// every command with ctx and closes the connection when it is done.
func ConnectReplication(ctx context.Context, cfg Config) (*pgconn.PgConn, error) {
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
