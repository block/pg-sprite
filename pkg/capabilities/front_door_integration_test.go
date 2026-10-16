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

// migrateClaim is one row's published promise that the migrate front door
// executes the operation: the schema it needs, and the statements that
// exercise it. %[1]s is the fixture schema, already safely named.
type migrateClaim struct {
	setup      string
	statements []string
	// minMajor is the oldest PostgreSQL major the row claims support on;
	// zero means every supported major.
	minMajor int
}

// migrateClaims maps every capability row whose imperative `migrate` door is
// published as supported to statements that must run through the real
// migrate.Run (gate, classification, routing, preflight, and sequence
// admission). A row the engine refuses cannot be published as supported:
// consumers read the matrix to decide whether to submit the statement.
var migrateClaims = map[string]migrateClaim{
	"add-column-no-default-or-constant-default": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ADD COLUMN nickname text`,
			`ALTER TABLE %[1]s.accounts ADD COLUMN tier int NOT NULL DEFAULT 0`,
		},
	},
	"drop-column": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				legacy_id int
			)`,
		statements: []string{`ALTER TABLE %[1]s.accounts DROP COLUMN legacy_id`},
	},
	"alter-column-type-binary-coercible-proven-against-live-column-facts": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				nickname varchar(50)
			)`,
		statements: []string{`ALTER TABLE %[1]s.accounts ALTER COLUMN nickname TYPE varchar(100)`},
	},
	"set-default-drop-default-drop-not-null": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				nickname text NOT NULL
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ALTER COLUMN nickname SET DEFAULT 'anon'`,
			`ALTER TABLE %[1]s.accounts ALTER COLUMN nickname DROP DEFAULT`,
			`ALTER TABLE %[1]s.accounts ALTER COLUMN nickname DROP NOT NULL`,
		},
	},
	"set-not-null": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				nickname text
			)`,
		statements: []string{`ALTER TABLE %[1]s.accounts ALTER COLUMN nickname SET NOT NULL`},
	},
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
	"add-primary-key-add-unique-plain-key-columns": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint NOT NULL,
				email text NOT NULL
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ADD PRIMARY KEY (id)`,
			`ALTER TABLE %[1]s.accounts ADD CONSTRAINT accounts_email_key UNIQUE (email)`,
		},
	},
	"add-check-add-foreign-key-imperative": {
		setup: `
			CREATE TABLE %[1]s.owners (
				id bigint PRIMARY KEY
			);
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				owner_id bigint,
				balance int
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ADD CONSTRAINT balance_nonnegative CHECK (balance >= 0)`,
			`ALTER TABLE %[1]s.accounts ADD CONSTRAINT accounts_owner_fk FOREIGN KEY (owner_id) REFERENCES %[1]s.owners (id)`,
		},
	},
	"add-constraint-not-valid-using-index-validate-constraint": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				balance int
			)`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ADD CONSTRAINT balance_nonnegative CHECK (balance >= 0) NOT VALID`,
			`ALTER TABLE %[1]s.accounts VALIDATE CONSTRAINT balance_nonnegative`,
		},
	},
	"add-foreign-key-not-valid-on-a-partitioned-parent": {
		setup: `
			CREATE TABLE %[1]s.owners (
				id bigint PRIMARY KEY
			);
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				owner_id bigint,
				PRIMARY KEY (id)
			) PARTITION BY RANGE (id)`,
		statements: []string{
			`ALTER TABLE %[1]s.events ADD CONSTRAINT events_owner_fk FOREIGN KEY (owner_id) REFERENCES %[1]s.owners (id) NOT VALID`,
		},
		minMajor: 18,
	},
	"drop-constraint": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				balance int,
				CONSTRAINT balance_nonnegative CHECK (balance >= 0)
			)`,
		statements: []string{`ALTER TABLE %[1]s.accounts DROP CONSTRAINT balance_nonnegative`},
	},
	"create-unique-index-on-a-plain-table-including-partial-expression-covering-include-gin-gist-brin": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				email text NOT NULL,
				deleted_at timestamptz
			)`,
		statements: []string{
			`CREATE UNIQUE INDEX accounts_email_live_idx ON %[1]s.accounts (lower(email)) WHERE deleted_at IS NULL`,
		},
	},
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
	"detach-partition-concurrently": {
		setup: `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				PRIMARY KEY (id)
			) PARTITION BY RANGE (id);
			CREATE TABLE %[1]s.events_low PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (1000);
			CREATE TABLE %[1]s.events_high PARTITION OF %[1]s.events FOR VALUES FROM (1000) TO (2000)`,
		statements: []string{
			`ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_low`,
			`ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_high CONCURRENTLY`,
		},
	},
}

// declarativeOnlyClaims are the rows whose migrate door is the desired-state
// path (migrate --desired), not an imperative statement; their execution is
// covered by the desired-state integration tests in pkg/migrate.
var declarativeOnlyClaims = []string{
	"plain-tables-their-indexes",
	"greenfield-create-table-apply-the-table-does-not-exist-yet-a-fresh-database-or-a-new-table-in-a-live-one",
	"declarative-row-security-library",
}

// Every capability row that publishes `front_doors.migrate: supported` must
// have its representative statements actually executed by migrate.Run. A
// refusal at the gate, the router, preflight, or sequence admission means the
// matrix promised an operation the shipped migrate command rejects.
func TestMigrateSupportedRowsExecute(t *testing.T) {
	rows, err := capabilities.Rows()
	require.NoError(t, err)

	var supported []string
	for _, row := range rows {
		if row.FrontDoors.Migrate == capabilities.DoorSupported {
			supported = append(supported, row.ID)
		}
	}
	for _, id := range supported {
		_, imperative := migrateClaims[id]
		assert.True(t, imperative || slices.Contains(declarativeOnlyClaims, id),
			"row %s publishes migrate: supported but has no representative statement in migrateClaims", id)
	}

	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)

	for _, id := range supported {
		claim, ok := migrateClaims[id]
		if !ok {
			continue
		}
		t.Run(id, func(t *testing.T) {
			if major < claim.minMajor {
				t.Skipf("row claims support from PostgreSQL %d; server is %d", claim.minMajor, major)
			}
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(claim.setup, schema))
			require.NoError(t, err)

			for _, sql := range claim.statements {
				sql = fmt.Sprintf(sql, schema)
				st, err := statement.ParseOne(sql)
				require.NoError(t, err)
				v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
				require.NoError(t, err, sql)
				assert.Equal(t, verdict.OutcomeExecuted, v.Outcome,
					"%s: the matrix publishes migrate as supported but it answered %s (%s): %s", sql, v.Outcome, v.Reason, v.Detail)
			}
		})
	}
}
