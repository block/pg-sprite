package decode

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/block/pg-sprite/pkg/dbconn"
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

// proveSameServer proves that the replication connection and the catalog
// session — a pool, or one connection of the caller's — are sessions of
// one database on one cluster, so a slot command issued on the connection
// acts on the cluster whose catalog the session reads, and returns that
// shared identity. The connection's identity comes from IDENTIFY_SYSTEM
// and the session's from pg_control_system() and current_database(); a
// server that answers either with an error fails the proof rather than
// passing it.
func proveSameServer(ctx context.Context, conn *pgconn.PgConn, catalog dbconn.RowQuerier) (serverIdentity, error) {
	replication, err := pglogrepl.IdentifySystem(ctx, conn)
	if err != nil {
		return serverIdentity{}, fmt.Errorf("identify the replication connection's server: %w", err)
	}
	var catalogued serverIdentity
	err = catalog.QueryRow(ctx, `
		SELECT s.system_identifier::text, pg_catalog.current_database()
		FROM pg_catalog.pg_control_system() s`).Scan(&catalogued.systemID, &catalogued.database)
	if err != nil {
		return serverIdentity{}, fmt.Errorf("identify the catalog session's server: %w", err)
	}
	// INV: ST-3 — the slot is named for the catalog session's database; a
	// replication connection on any other cluster, or any other database
	// of this one, would create, drop, or decode a slot the name does not
	// belong to.
	if replication.SystemID != catalogued.systemID {
		return serverIdentity{}, fmt.Errorf("%w: ST-3: replication connection is on cluster %s, the catalog session on cluster %s",
			ErrInvariantViolation, replication.SystemID, catalogued.systemID)
	}
	if replication.DBName != catalogued.database {
		return serverIdentity{}, fmt.Errorf("%w: ST-3: replication connection is on database %q, the catalog session on %q",
			ErrInvariantViolation, replication.DBName, catalogued.database)
	}
	return catalogued, nil
}
