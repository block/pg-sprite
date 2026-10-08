package dbconn

import (
	"context"
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
	// A walsender reports what it will withhold from the stream as a
	// warning. A startup parameter outranks the role's, the database's, and
	// the server's client_min_messages, so none of them can keep that
	// warning from reaching the connection.
	connConfig.RuntimeParams["client_min_messages"] = "warning"
	conn, err := pgconn.ConnectConfig(ctx, &connConfig.Config)
	if err != nil {
		return nil, fmt.Errorf("connect replication session: %w", err)
	}
	return conn, nil
}
