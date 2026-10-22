package capabilities_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/capabilities"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// migrateRefusals maps rows published as `migrate: refused` to statements the
// migrate front door must refuse with the row's refusal_reason. Consumers
// filter on the published token, so it must be the one migrate emits.
var migrateRefusals = map[string]migrateClaim{
	// The attach scans the partition to prove its bound, and the engine
	// does not yet construct the CHECK that would let it skip the scan.
	"attach-partition": {
		setup: `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				PRIMARY KEY (id)
			) PARTITION BY RANGE (id);
			CREATE TABLE %[1]s.events_low (
				id bigint NOT NULL,
				PRIMARY KEY (id)
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.events ATTACH PARTITION %[1]s.events_low FOR VALUES FROM (0) TO (1000)`,
		},
	},
	// The plain form is rewritten to CONCURRENTLY, and the executor does not
	// drive a concurrent detach; both forms refuse the same way.
	"detach-partition-concurrently": {
		setup: `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				PRIMARY KEY (id)
			) PARTITION BY RANGE (id);
			CREATE TABLE %[1]s.events_low PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (1000)`,
		statements: []string{
			`ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_low`,
			`ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_low CONCURRENTLY`,
		},
	},
	// The imperative door does not drive index maintenance: the plain form
	// names its concurrent idiom, and the concurrent form is run directly.
	"drop-index": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				email text NOT NULL
			);
			CREATE INDEX accounts_email_idx ON %[1]s.accounts (email)`,
		statements: []string{
			`DROP INDEX %[1]s.accounts_email_idx`,
			`DROP INDEX CONCURRENTLY %[1]s.accounts_email_idx`,
		},
	},
	"reindex": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				email text NOT NULL
			);
			CREATE INDEX accounts_email_idx ON %[1]s.accounts (email)`,
		statements: []string{
			`REINDEX INDEX %[1]s.accounts_email_idx`,
			`REINDEX INDEX CONCURRENTLY %[1]s.accounts_email_idx`,
		},
	},
	// The imperative door takes only ALTER TABLE and CREATE INDEX, so an
	// ALTER TYPE is refused at the gate.
	"enum-value-rename-removal": {
		setup: `CREATE TYPE %[1]s.mood AS ENUM ('happy', 'sad')`,
		statements: []string{
			`ALTER TYPE %[1]s.mood RENAME VALUE 'sad' TO 'blue'`,
		},
	},
	// A rename is a brief catalog change for PostgreSQL but breaks every
	// application instance still using the old name the moment it commits;
	// no safer SQL exists, so without --force it is refused with the rollout
	// the application can follow. The refused column rename leaves the
	// table untouched, so the table rename finds it under its name.
	"rename-column-rename-table": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				nickname text
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts RENAME COLUMN nickname TO display_name`,
			`ALTER TABLE %[1]s.accounts RENAME TO members`,
		},
	},
	// The inline constraint would build its index under the ADD COLUMN's
	// ACCESS EXCLUSIVE lock; the safe form adds the column first.
	"add-column-with-inline-unique-primary-key-references-check": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ADD COLUMN code int UNIQUE`,
		},
	},
}

// A row published as `migrate: refused` must be refused by migrate.Run with
// exactly the published refusal_reason.
func TestMigrateRefusedRowsRefuse(t *testing.T) {
	rows, err := capabilities.Rows()
	require.NoError(t, err)
	byID := map[string]capabilities.Row{}
	for _, row := range rows {
		byID[row.ID] = row
	}

	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	for id, claim := range migrateRefusals {
		t.Run(id, func(t *testing.T) {
			row, ok := byID[id]
			require.True(t, ok, "row %s is not in the registry", id)
			require.Equal(t, capabilities.DoorRefused, row.FrontDoors.Migrate)
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(claim.setup, schema))
			require.NoError(t, err)

			for _, sql := range claim.statements {
				sql = fmt.Sprintf(sql, schema)
				st, err := statement.ParseOne(sql)
				require.NoError(t, err)
				v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
				require.NoError(t, err, sql)
				assert.Equal(t, verdict.OutcomeRefused, v.Outcome, sql)
				assert.Equal(t, row.RefusalReason, v.Reason,
					"%s: the matrix publishes a different reason than migrate emits", sql)
			}
		})
	}
}

// A representative statement is only evidence for a row the matrix publishes
// as supported.
func TestMigrateClaimsBelongToSupportedRows(t *testing.T) {
	rows, err := capabilities.Rows()
	require.NoError(t, err)
	for id := range migrateClaims {
		i := slices.IndexFunc(rows, func(r capabilities.Row) bool { return r.ID == id })
		require.GreaterOrEqual(t, i, 0, "row %s is not in the registry", id)
		assert.Equal(t, capabilities.DoorSupported, rows[i].FrontDoors.Migrate,
			"migrateClaims has statements for row %s, which is not published as migrate: supported", id)
	}
}
