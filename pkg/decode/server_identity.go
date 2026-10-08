package decode

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// serverIdentity is what tells one database session's server from
// another's: the cluster's system identifier, minted once by initdb and
// shared by no two clusters, and the database name, which is unique only
// within a cluster. A name alone cannot tell two clusters that each hold a
// database of the name apart.
type serverIdentity struct {
	systemID string
	database string
}

// proveSameServer proves that the replication connection and the pool are
// sessions of one database on one cluster, so a slot command issued on the
// connection acts on the cluster whose catalog the pool reads. The
// connection's identity comes from IDENTIFY_SYSTEM and the pool's from
// pg_control_system() and current_database(); a server that answers either
// with an error fails the proof rather than passing it.
func proveSameServer(ctx context.Context, conn *pgconn.PgConn, pool *pgxpool.Pool) error {
	replication, err := pglogrepl.IdentifySystem(ctx, conn)
	if err != nil {
		return fmt.Errorf("identify the replication connection's server: %w", err)
	}
	var pooled serverIdentity
	err = pool.QueryRow(ctx, `
		SELECT s.system_identifier::text, pg_catalog.current_database()
		FROM pg_catalog.pg_control_system() s`).Scan(&pooled.systemID, &pooled.database)
	if err != nil {
		return fmt.Errorf("identify the pool's server: %w", err)
	}
	// INV: ST-3 — the slot is named for the pool's database; a replication
	// connection on any other cluster, or any other database of this one,
	// would create or drop a slot the name does not belong to.
	if replication.SystemID != pooled.systemID {
		return fmt.Errorf("%w: ST-3: replication connection is on cluster %s, the pool on cluster %s",
			ErrInvariantViolation, replication.SystemID, pooled.systemID)
	}
	if replication.DBName != pooled.database {
		return fmt.Errorf("%w: ST-3: replication connection is on database %q, the pool on %q",
			ErrInvariantViolation, replication.DBName, pooled.database)
	}
	return nil
}
