package migrate_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/capabilities"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// noRewriteCase is one statement the imperative front door executes
// natively, run against a table built by setup. Each statement names the
// fixture schema as %[1]s.
type noRewriteCase struct {
	name      string
	setup     []string
	statement string
	// minMajor is the first server major version the row supports the
	// statement on; below it the run must refuse with belowMinReason,
	// having executed nothing.
	minMajor       int
	belowMinReason verdict.Reason
}

// noRewriteFixture is the table most cases start from: rows in it, so a
// rewrite has something to copy, and a referenced table for foreign keys.
var noRewriteFixture = []string{
	"CREATE TABLE %[1]s.parent (id int PRIMARY KEY)",
	"INSERT INTO %[1]s.parent SELECT generate_series(1, 100)",
	"CREATE TABLE %[1]s.t (id int NOT NULL, email varchar(50) NOT NULL, note text DEFAULT 'x', qty int, parent_id int, tags text[], amount numeric(10,2), span int4range)",
	"INSERT INTO %[1]s.t SELECT g, 'u' || g, NULL, g, g, ARRAY['a'], g * 1.25, int4range(g, g + 10) FROM generate_series(1, 100) g",
}

// noRewritePartitioned is a partitioned table with one partition and a
// standalone table that fits a second partition's bound.
var noRewritePartitioned = []string{
	"CREATE TABLE %[1]s.parent (id int PRIMARY KEY)",
	"INSERT INTO %[1]s.parent SELECT generate_series(1, 100)",
	"CREATE TABLE %[1]s.t (id int NOT NULL, parent_id int) PARTITION BY RANGE (id)",
	"CREATE TABLE %[1]s.t_low PARTITION OF %[1]s.t FOR VALUES FROM (0) TO (50)",
	"INSERT INTO %[1]s.t SELECT g, g FROM generate_series(1, 49) g",
	"CREATE TABLE %[1]s.t_high (id int NOT NULL, parent_id int)",
	"INSERT INTO %[1]s.t_high SELECT g, g FROM generate_series(50, 99) g",
}

// noRewriteCases holds, for every capability row whose imperative front
// door executes natively, statements of that row's shape. The row's claim
// is that the statement runs without rewriting the table, so each case
// reads the filenode of every table in the fixture schema before and
// after and fails on any change.
var noRewriteCases = map[string][]noRewriteCase{
	"add-column-no-default-or-constant-default": {
		{name: "no default", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD COLUMN c int"},
		{name: "constant default", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD COLUMN c text NOT NULL DEFAULT 'active'"},
		{name: "stable default", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD COLUMN c timestamptz DEFAULT now()"},
	},
	"add-constraint-not-valid-using-index-validate-constraint": {
		{name: "check not valid", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID"},
		{
			name:      "validate constraint",
			setup:     append(slices.Clone(noRewriteFixture), "ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID"),
			statement: "ALTER TABLE %[1]s.t VALIDATE CONSTRAINT t_qty_check",
		},
		{
			name:      "unique using index",
			setup:     append(slices.Clone(noRewriteFixture), "CREATE UNIQUE INDEX t_email_key ON %[1]s.t (email)"),
			statement: "ALTER TABLE %[1]s.t ADD CONSTRAINT t_email_key UNIQUE USING INDEX t_email_key",
		},
	},
	"alter-column-type-binary-coercible-proven-against-live-column-facts": {
		{name: "widen varchar", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ALTER COLUMN email TYPE varchar(100)"},
		{name: "varchar to text", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ALTER COLUMN email TYPE text"},
		{name: "widen numeric precision", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ALTER COLUMN amount TYPE numeric(12,2)"},
	},
	"declarative-not-valid-constraint": {
		{
			name:      "validate a check left not valid",
			setup:     append(slices.Clone(noRewriteFixture), "ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID"),
			statement: "ALTER TABLE %[1]s.t VALIDATE CONSTRAINT t_qty_check",
		},
		{
			name:      "validate a foreign key left not valid",
			setup:     append(slices.Clone(noRewriteFixture), "ALTER TABLE %[1]s.t ADD CONSTRAINT t_parent_fk FOREIGN KEY (parent_id) REFERENCES %[1]s.parent (id) NOT VALID"),
			statement: "ALTER TABLE %[1]s.t VALIDATE CONSTRAINT t_parent_fk",
		},
	},
	"drop-column": {
		{name: "drop column", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t DROP COLUMN note"},
	},
	"drop-constraint": {
		{
			name:      "drop check",
			setup:     append(slices.Clone(noRewriteFixture), "ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0)"),
			statement: "ALTER TABLE %[1]s.t DROP CONSTRAINT t_qty_check",
		},
	},
	"plain-tables-their-indexes": {
		{name: "create index", setup: noRewriteFixture, statement: "CREATE INDEX t_qty_idx ON %[1]s.t (qty)"},
	},
	"set-default-drop-default-drop-not-null": {
		{name: "set default", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ALTER COLUMN qty SET DEFAULT 1"},
		{name: "drop default", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ALTER COLUMN note DROP DEFAULT"},
		{name: "drop not null", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ALTER COLUMN email DROP NOT NULL"},
	},
	"add-check-add-foreign-key-imperative": {
		{name: "add check", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0)"},
		{name: "add foreign key", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD CONSTRAINT t_parent_fk FOREIGN KEY (parent_id) REFERENCES %[1]s.parent (id)"},
	},
	"add-foreign-key-not-valid-on-a-partitioned-parent": {
		{
			name:           "foreign key on a partitioned parent",
			setup:          noRewritePartitioned,
			statement:      "ALTER TABLE %[1]s.t ADD CONSTRAINT t_parent_fk FOREIGN KEY (parent_id) REFERENCES %[1]s.parent (id) NOT VALID",
			minMajor:       18,
			belowMinReason: verdict.ReasonUnsupportedPartitionedParent,
		},
	},
	"add-primary-key-add-unique-plain-key-columns": {
		{name: "add primary key", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD CONSTRAINT t_pkey PRIMARY KEY (id)"},
		{name: "add unique", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ADD CONSTRAINT t_email_key UNIQUE (email)"},
	},
	"create-unique-index-on-a-plain-table-including-partial-expression-covering-include-gin-gist-brin": {
		{name: "unique index", setup: noRewriteFixture, statement: "CREATE UNIQUE INDEX t_email_idx ON %[1]s.t (email)"},
		{name: "partial index", setup: noRewriteFixture, statement: "CREATE INDEX t_qty_idx ON %[1]s.t (qty) WHERE qty > 10"},
		{name: "expression index", setup: noRewriteFixture, statement: "CREATE INDEX t_lower_email_idx ON %[1]s.t (lower(email))"},
		{name: "covering index", setup: noRewriteFixture, statement: "CREATE INDEX t_qty_idx ON %[1]s.t (qty) INCLUDE (email)"},
		{name: "gin index", setup: noRewriteFixture, statement: "CREATE INDEX t_tags_idx ON %[1]s.t USING gin (tags)"},
		{name: "gist index", setup: noRewriteFixture, statement: "CREATE INDEX t_span_idx ON %[1]s.t USING gist (span)"},
		{name: "brin index", setup: noRewriteFixture, statement: "CREATE INDEX t_id_brin ON %[1]s.t USING brin (id)"},
	},
	"set-not-null": {
		{name: "set not null", setup: noRewriteFixture, statement: "ALTER TABLE %[1]s.t ALTER COLUMN qty SET NOT NULL"},
	},
}

// noRewriteNotApplicable names the natively executed rows a heap filenode
// cannot speak to, and why.
var noRewriteNotApplicable = map[string]string{
	"greenfield-create-table-apply-the-table-does-not-exist-yet-a-fresh-database-or-a-new-table-in-a-live-one": "the table does not exist before the statement, so there is no heap to rewrite",
	"declarative-row-security-library": "changes only policies and the table's row-security flags, through the row-security executor rather than a statement",
}

// Every capability row whose imperative front door executes natively makes
// a claim about the table's storage, so every such row has to be proven
// against the server below or be named as one a filenode cannot speak to.
// A row added later fails here until it is classified, and a case keyed
// on a row that no longer runs natively fails as stale.
func TestNoRewriteCasesCoverEveryNativeRow(t *testing.T) {
	rows, err := capabilities.Rows()
	require.NoError(t, err)
	native := map[string]bool{}
	for _, row := range rows {
		if executesNatively(row) {
			native[row.ID] = true
		}
	}
	for id := range native {
		_, proven := noRewriteCases[id]
		_, exempt := noRewriteNotApplicable[id]
		assert.True(t, proven != exempt, "row %s needs no-rewrite cases or a reason a filenode cannot speak to it, not both", id)
	}
	for id, cases := range noRewriteCases {
		assert.True(t, native[id], "no-rewrite cases for %s, which is not a natively executed row", id)
		assert.NotEmpty(t, cases, "row %s is classified as proven but has no cases to run", id)
	}
	for id, reason := range noRewriteNotApplicable {
		assert.True(t, native[id], "no-rewrite exemption for %s, which is not a natively executed row", id)
		assert.NotEmpty(t, reason, "row %s is exempt without a reason", id)
	}
}

// executesNatively reports whether the imperative front door runs the row's
// statements natively, as written or as the safer sequence.
func executesNatively(row capabilities.Row) bool {
	if row.FrontDoors.Migrate != capabilities.DoorSupported {
		return false
	}
	return row.EnginePath == capabilities.PathNativeAsIs || row.EnginePath == capabilities.PathNativeSaferSequence
}

// A natively executed statement runs under the engine's lock budgets, so a
// statement the server answers with a full rewrite would block reads and
// writes for as long as the copy takes. Each case runs its statement
// through migrate.Run and requires that the run executed and that no table
// in the fixture schema got a new filenode.
func TestNativeRowsRunWithoutARewrite(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var major int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int / 10000").Scan(&major))

	for id, cases := range noRewriteCases {
		for _, tc := range cases {
			t.Run(id+"/"+tc.name, func(t *testing.T) {
				schema := testutil.NewSchema(t, pool)
				for _, ddl := range tc.setup {
					_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
					require.NoError(t, err, ddl)
				}
				before := heapFilenodes(t, pool, schema)

				sql := fmt.Sprintf(tc.statement, schema)
				v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), runOptions())
				require.NoError(t, err)
				switch {
				case major < tc.minMajor:
					require.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s on PostgreSQL %d: %s %s", sql, major, v.Reason, v.Detail)
					assert.Equal(t, tc.belowMinReason, v.Reason, "%s on PostgreSQL %d", sql, major)
				default:
					require.Equal(t, verdict.OutcomeExecuted, v.Outcome, "%s: %s %s", sql, v.Reason, v.Detail)
				}

				assert.Equal(t, before, heapFilenodes(t, pool, schema), "%s rewrote a table; executed %v", sql, v.ExecutedSQL)
			})
		}
	}
}

// heapFilenodes maps each table in the schema that has storage, by oid so a
// rename does not read as a change, to its filenode.
func heapFilenodes(t *testing.T, pool *pgxpool.Pool, schema string) map[uint32]uint32 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT c.oid, pg_catalog.pg_relation_filenode(c.oid)
		FROM pg_catalog.pg_class c
		WHERE c.relnamespace = $1::regnamespace AND c.relkind = 'r'`, schema)
	require.NoError(t, err)
	defer rows.Close()
	out := map[uint32]uint32{}
	for rows.Next() {
		var oid, node uint32
		require.NoError(t, rows.Scan(&oid, &node))
		out[oid] = node
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out, "the fixture has tables with storage")
	return out
}
