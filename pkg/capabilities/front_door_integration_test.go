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
	// copyAndSwap marks a row whose statements the router sends through
	// the copy-and-swap backend: the run is given the route's policy (its
	// connection, disk headroom, and the quiesced declaration, since the
	// fixture server has no logical decoding) and its executed outcome is
	// the route's own token.
	copyAndSwap bool
	// destructive marks a row whose statements discard live data or
	// structure: migrate runs them only under the destructive
	// acknowledgement, which the run names for the fixture's accounts
	// table, the table every such row's statements change.
	destructive bool
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
	// A plain index renames as a catalog change on the index alone; the
	// statement names no table, so migrate resolves the index's owner.
	"alter-index-rename": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				email text NOT NULL
			);
			CREATE INDEX accounts_email_idx ON %[1]s.accounts (email)`,
		statements: []string{`ALTER INDEX %[1]s.accounts_email_idx RENAME TO accounts_contact_email_idx`},
	},
	"drop-column": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				legacy_id int
			)`,
		statements:  []string{`ALTER TABLE %[1]s.accounts DROP COLUMN legacy_id`},
		destructive: true,
	},
	"alter-column-type-binary-coercible-proven-against-live-column-facts": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				nickname varchar(50)
			)`,
		statements: []string{`ALTER TABLE %[1]s.accounts ALTER COLUMN nickname TYPE varchar(100)`},
	},
	"alter-column-type-general-or-with-using": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id      bigint PRIMARY KEY,
				balance int
			);
			INSERT INTO %[1]s.accounts SELECT g, g FROM generate_series(1, 100) g`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ALTER COLUMN balance TYPE bigint`,
			`ALTER TABLE %[1]s.accounts ALTER COLUMN balance TYPE text USING balance::text`,
		},
		copyAndSwap: true,
	},
	"column-transform-expressions-during-a-copy-and-swap-rewrite": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id    bigint PRIMARY KEY,
				price numeric(10, 2)
			);
			INSERT INTO %[1]s.accounts SELECT g, g * 1.25 FROM generate_series(1, 100) g`,
		statements: []string{
			`ALTER TABLE %[1]s.accounts ALTER COLUMN price TYPE integer USING (price * 100)::integer`,
		},
		copyAndSwap: true,
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
		destructive: true,
	},
	"set-not-null": {
		setup: `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY,
				nickname text
			)`,
		statements: []string{`ALTER TABLE %[1]s.accounts ALTER COLUMN nickname SET NOT NULL`},
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
		statements:  []string{`ALTER TABLE %[1]s.accounts DROP CONSTRAINT balance_nonnegative`},
		destructive: true,
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
}

// declarativeOnlyClaims are the rows whose migrate door is the desired-state
// path (migrate --desired), not an imperative statement; their execution is
// covered by the desired-state integration tests in pkg/migrate.
var declarativeOnlyClaims = []string{
	"plain-tables-their-indexes",
	"greenfield-create-table-apply-the-table-does-not-exist-yet-a-fresh-database-or-a-new-table-in-a-live-one",
	"declarative-row-security-library",
	"declarative-not-valid-constraint",
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

			opts := migrate.DefaultOptions()
			if claim.destructive {
				opts.AllowDestructive = schema + ".accounts"
			}
			executed := verdict.OutcomeExecuted
			if claim.copyAndSwap {
				opts.CopySwap = migrate.CopySwapOptions{
					Connection:    dbconn.Config{URL: url},
					FreeDiskBytes: testutil.UnlimitedDisk,
				}
				executed = verdict.OutcomeExecutedViaCopyAndSwap
			}
			for _, sql := range claim.statements {
				sql = fmt.Sprintf(sql, schema)
				st, err := statement.ParseOne(sql)
				require.NoError(t, err)
				if claim.copyAndSwap {
					opts.CopySwap.BlockWrites = st.Schema() + "." + st.Table()
				}
				v, err := migrate.Run(t.Context(), pool, st, opts)
				require.NoError(t, err, sql)
				assert.Equal(t, executed, v.Outcome,
					"%s: the matrix publishes migrate as supported but it answered %s (%s): %s", sql, v.Outcome, v.Reason, v.Detail)
			}
		})
	}
}
