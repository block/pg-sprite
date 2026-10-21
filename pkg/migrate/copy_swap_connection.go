package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// CopySwapConnectionMismatchError is returned by [Run] and [RunDesired]
// when Options.CopySwap.Connection reaches a database other than the one
// the pool handed to Run was dialed with. The route opens its lock
// session from the connection and runs every other statement on the pool,
// and each working transaction confirms the table lock from the pool it
// runs on; a pool on another database would see no lock and refuse as
// shadow-lock-unconfirmed, an environmental refusal an orchestrator
// retries, although no retry corrects a wiring mistake. The check runs
// before the preflight, so nothing has been executed when it is returned;
// match it with [errors.As] and fix the wiring rather than routing it as
// an outcome.
//
// A database is told from another by the cluster's system identifier,
// minted once by initdb and shared by no two clusters, together with the
// database name, which is unique only within a cluster.
type CopySwapConnectionMismatchError struct {
	// PoolCluster and PoolDatabase identify the database the pool reaches.
	PoolCluster  string
	PoolDatabase string
	// ConnectionCluster and ConnectionDatabase identify the database
	// Options.CopySwap.Connection reaches.
	ConnectionCluster  string
	ConnectionDatabase string
}

// Error implements the error interface.
func (e *CopySwapConnectionMismatchError) Error() string {
	if e.PoolCluster != e.ConnectionCluster {
		return fmt.Sprintf("Options.CopySwap.Connection reaches database %q on cluster %s, the pool database %q on cluster %s; both must name the database the pool was dialed with",
			e.ConnectionDatabase, e.ConnectionCluster, e.PoolDatabase, e.PoolCluster)
	}
	return fmt.Sprintf("Options.CopySwap.Connection reaches database %q, the pool database %q on the same cluster; both must name the database the pool was dialed with",
		e.ConnectionDatabase, e.PoolDatabase)
}

// databaseIdentity is what tells one session's database from another's:
// the cluster's system identifier and the database name within it.
type databaseIdentity struct {
	cluster  string
	database string
}

// databaseIdentitySQL names the cluster and the database a session is on.
const databaseIdentitySQL = `
	SELECT s.system_identifier::text, pg_catalog.current_database()
	FROM pg_catalog.pg_control_system() s`

// readDatabaseIdentity reads the identity of the database session reaches.
func readDatabaseIdentity(ctx context.Context, session dbconn.RowQuerier) (databaseIdentity, error) {
	var id databaseIdentity
	err := session.QueryRow(ctx, databaseIdentitySQL).Scan(&id.cluster, &id.database)
	return id, err
}

// checkCopySwapConnection proves that cfg and pool are sessions of one
// database on one cluster before the route opens anything from cfg. It
// dials one session from cfg, as the route's lock session would be dialed,
// reads both identities, and closes the session again. A disagreement is a
// *CopySwapConnectionMismatchError; a session that could not be opened or
// identified is an ordinary error, since nothing is known about where it
// would have landed.
func checkCopySwapConnection(ctx context.Context, pool *pgxpool.Pool, cfg dbconn.Config) (err error) {
	poolID, err := readDatabaseIdentity(ctx, pool)
	if err != nil {
		return fmt.Errorf("identify the pool's database: %w", err)
	}
	conn, err := dbconn.ConnectSession(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open a session from Options.CopySwap.Connection: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(context.WithoutCancel(ctx)); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the session opened from Options.CopySwap.Connection: %w", closeErr))
		}
	}()
	connID, err := readDatabaseIdentity(ctx, conn)
	if err != nil {
		return fmt.Errorf("identify the database Options.CopySwap.Connection reaches: %w", err)
	}
	if connID != poolID {
		return &CopySwapConnectionMismatchError{
			PoolCluster:        poolID.cluster,
			PoolDatabase:       poolID.database,
			ConnectionCluster:  connID.cluster,
			ConnectionDatabase: connID.database,
		}
	}
	return nil
}
