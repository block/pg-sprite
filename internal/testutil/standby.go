package testutil

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// standbyCaughtUpDeadline bounds how long a fresh standby gets to
	// replay the primary's position read once it accepts connections.
	standbyCaughtUpDeadline = 30 * time.Second
	standbyCaughtUpPoll     = 100 * time.Millisecond
)

// standbyReplicationHBA lets the standby's pg_basebackup and its WAL
// receiver connect to the primary over the fixture's network. The image's
// own pg_hba.conf admits replication connections only from the loopback
// address; the script runs before the primary's final start, so the entry
// is in force when the standby connects.
const standbyReplicationHBA = `#!/bin/sh
set -e
echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"
`

// PrimaryStandby is a primary and a hot standby streaming from it, each
// in a container of its own.
type PrimaryStandby struct {
	PrimaryURL string
	StandbyURL string
}

// StartPrimaryWithStandby starts a primary, then a hot standby cloned
// from it by pg_basebackup and streaming its WAL, and returns a
// connection URL for each. settings (each "name=value") go on both
// servers' command lines, so a setting the standby checks against its own
// configuration, such as wal_level for logical decoding, holds on both.
// It returns once the standby reports it is in recovery and has replayed
// the primary's write position as it stood once the standby was up, so a
// caller reads a standby that is in recovery and current.
//
// It always starts its own containers: a replication pair is the fixture,
// so an external PG_DSN cannot substitute for it. Set SKIP_INTEGRATION=1
// to skip.
func StartPrimaryWithStandby(t *testing.T, settings ...string) PrimaryStandby {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs a database")
	}
	const (
		user     = "postgres"
		password = "postgres"
		database = "postgres"
		alias    = "primary"
		dataDir  = "/tmp/standby"
	)
	args := make([]string, 0, 2*len(settings))
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	ctx := t.Context()

	net, err := tcnetwork.New(ctx)
	require.NoError(t, err, "create the replication test network")
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation, or
		// the removal always fails and each run leaks a bridge network.
		if err := net.Remove(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("remove the replication test network: %v", err)
		}
	})

	primary, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:" + PGVersion(),
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     user,
				"POSTGRES_PASSWORD": password,
				"POSTGRES_DB":       database,
			},
			Cmd:            append([]string{"postgres"}, args...),
			Networks:       []string{net.Name},
			NetworkAliases: map[string][]string{net.Name: {alias}},
			Files: []testcontainers.ContainerFile{{
				Reader:            strings.NewReader(standbyReplicationHBA),
				ContainerFilePath: "/docker-entrypoint-initdb.d/000-replication-hba.sh",
				FileMode:          0o755,
			}},
			// The image starts the server once to initialise it and again
			// to serve; only the second start is the one the standby can
			// clone from.
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2),
		},
		Started: true,
	})
	require.NoError(t, err, "start the primary postgres container")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(primary); err != nil {
			t.Logf("terminate the primary postgres container: %v", err)
		}
	})

	// pg_basebackup -R writes the standby.signal file and the
	// primary_conninfo the server then streams from, so the clone starts
	// as a hot standby of the primary.
	clone := "pg_basebackup --host=" + alias + " --username=" + user +
		" --pgdata=" + dataDir + " --write-recovery-conf --wal-method=stream --checkpoint=fast" +
		` && exec postgres -D ` + dataDir + ` "$@"`
	standby, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:" + PGVersion(),
			ExposedPorts: []string{"5432/tcp"},
			Env:          map[string]string{"PGPASSWORD": password},
			Entrypoint:   append([]string{"bash", "-c", clone, "standby"}, args...),
			ConfigModifier: func(c *container.Config) {
				// The server refuses to run as root, and the clone's data
				// directory must belong to the user that runs it.
				c.User = user
			},
			Networks: []string{net.Name},
			WaitingFor: wait.ForAll(
				wait.ForLog("database system is ready to accept read-only connections"),
				wait.ForListeningPort("5432/tcp"),
			),
		},
		Started: true,
	})
	require.NoError(t, err, "start the standby postgres container")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(standby); err != nil {
			t.Logf("terminate the standby postgres container: %v", err)
		}
	})

	pair := PrimaryStandby{
		PrimaryURL: containerURL(t, primary, "5432", user, password, database),
		StandbyURL: containerURL(t, standby, "5432", user, password, database),
	}
	awaitStandbyReplayed(t, pair)
	return pair
}

// awaitStandbyReplayed polls until the standby is in recovery and has
// replayed the primary's current write position, read once before the
// poll starts.
func awaitStandbyReplayed(t *testing.T, pair PrimaryStandby) {
	t.Helper()
	primary, err := pgx.Connect(t.Context(), pair.PrimaryURL)
	require.NoError(t, err, "connect to the primary")
	defer func() {
		assert.NoError(t, primary.Close(context.WithoutCancel(t.Context())), "close the primary connection")
	}()
	var target string
	require.NoError(t, primary.QueryRow(t.Context(), `SELECT pg_catalog.pg_current_wal_lsn()::text`).Scan(&target),
		"read the primary's write position")

	standby, err := pgx.Connect(t.Context(), pair.StandbyURL)
	require.NoError(t, err, "connect to the standby")
	defer func() {
		assert.NoError(t, standby.Close(context.WithoutCancel(t.Context())), "close the standby connection")
	}()
	require.EventuallyWithTf(t, func(collect *assert.CollectT) {
		var inRecovery, replayed bool
		err := standby.QueryRow(t.Context(), `
			SELECT pg_catalog.pg_is_in_recovery(),
			       COALESCE(pg_catalog.pg_last_wal_replay_lsn() >= $1::pg_lsn, false)`, target).
			Scan(&inRecovery, &replayed)
		if !assert.NoError(collect, err, "read the standby's recovery state") {
			return
		}
		assert.True(collect, inRecovery, "the standby reports it is in recovery")
		assert.True(collect, replayed, "the standby has replayed the primary's position %s", target)
	}, standbyCaughtUpDeadline, standbyCaughtUpPoll,
		"the standby did not replay the primary's position %s within %s", target, standbyCaughtUpDeadline)
}
