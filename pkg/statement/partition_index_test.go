package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// OnlyIndex turns a request into the metadata-only parent statement: ONLY is
// set and CONCURRENTLY is gone, and the key, predicate, and uniqueness stay
// the request's. A request already written ON ONLY comes back unchanged.
func TestOnlyIndexClearsConcurrentlyAndSetsOnly(t *testing.T) {
	tests := []struct{ name, sql, want string }{
		{
			"concurrent unique with a predicate",
			"CREATE UNIQUE INDEX CONCURRENTLY idx_orders_region ON app.orders (region, id) WHERE region IS NOT NULL",
			"CREATE UNIQUE INDEX idx_orders_region ON ONLY app.orders USING btree (region, id) WHERE region IS NOT NULL",
		},
		{
			"already only",
			"CREATE INDEX idx_orders_total ON ONLY app.orders USING btree (total) INCLUDE (note)",
			"CREATE INDEX idx_orders_total ON ONLY app.orders USING btree (total) INCLUDE (note)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statement.OnlyIndex(tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// PartitionIndex retargets the parent's printed definition at a partition.
// The expression, collation, operator class, sort options, predicate, and
// storage parameters survive untouched, and a name that needs quoting comes
// back quoted.
func TestPartitionIndexRetargetsTheParentDefinition(t *testing.T) {
	const def = `CREATE INDEX idx_orders_name ON ONLY app.orders
		USING btree (lower(name::text) COLLATE "C" text_pattern_ops DESC NULLS LAST)
		WITH (fillfactor=70) WHERE total > 0`
	tests := []struct {
		name        string
		def         string
		schema, tbl string
		index       string
		form        statement.PartitionIndexForm
		want        string
	}{
		{
			"a leaf is built concurrently", def, "archive", "orders_2024", "idx_orders_name_p16401", statement.PartitionIndexConcurrently,
			`CREATE INDEX CONCURRENTLY idx_orders_name_p16401 ON archive.orders_2024 USING btree (lower(name::text) COLLATE "C" text_pattern_ops DESC NULLS LAST) WITH (fillfactor=70) WHERE total > 0`,
		},
		{
			"a sub-partitioned partition gets the metadata-only form",
			"CREATE UNIQUE INDEX idx_orders_pk ON ONLY app.orders USING btree (id, region)",
			"app", "orders_eu", "idx_orders_pk_p16402", statement.PartitionIndexOnly,
			"CREATE UNIQUE INDEX idx_orders_pk_p16402 ON ONLY app.orders_eu USING btree (id, region)",
		},
		{
			"names that need quoting",
			"CREATE INDEX idx ON ONLY app.orders USING btree (total)",
			"Odd Schema", "Orders 2024", "Idx_P1", statement.PartitionIndexConcurrently,
			`CREATE INDEX CONCURRENTLY "Idx_P1" ON "Odd Schema"."Orders 2024" USING btree (total)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statement.PartitionIndex(tt.def, tt.schema, tt.tbl, tt.index, tt.form)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Both edits refuse a statement they cannot edit by changing a few named
// fields, with a typed error, and surface a parse failure as it is.
func TestPartitionedIndexEditsRefuseUneditableStatements(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want error
	}{
		{"not an index", "ALTER TABLE app.orders ADD COLUMN note text", statement.ErrNotCreateIndex},
		{"if not exists", "CREATE INDEX IF NOT EXISTS idx_orders_total ON app.orders (total)", statement.ErrIndexIfNotExists},
		{"unnamed", "CREATE INDEX ON app.orders (total)", statement.ErrIndexUnnamed},
		{"two statements", "CREATE INDEX a ON app.orders (total); CREATE INDEX b ON app.orders (note)", statement.ErrNotOneStatement},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := statement.OnlyIndex(tt.sql)
			assert.ErrorIs(t, err, tt.want)
			_, err = statement.PartitionIndex(tt.sql, "app", "orders_1", "idx_p1", statement.PartitionIndexConcurrently)
			assert.ErrorIs(t, err, tt.want)
		})
	}

	_, err := statement.PartitionIndex("CREATE INDEX idx ON ONLY app.orders USING btree (total)", "app", "orders_1", "idx_p1", 0)
	assert.ErrorIs(t, err, statement.ErrUnknownPartitionIndexForm)

	_, err = statement.OnlyIndex("CREATE INDEX idx ON app.orders (")
	require.Error(t, err)
	assert.NotErrorIs(t, err, statement.ErrNotCreateIndex)
}

// IndexKeyColumns names the key's plain columns in key order. An expression
// key and an INCLUDE column are not key columns.
func TestIndexKeyColumnsNamesThePlainKeyColumns(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{"plain columns", "CREATE UNIQUE INDEX i ON app.events (id, ts)", []string{"id", "ts"}},
		{"an expression key", "CREATE UNIQUE INDEX i ON app.events (id, lower(region))", []string{"id"}},
		{"an include column", "CREATE UNIQUE INDEX i ON app.events (id) INCLUDE (ts)", []string{"id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statement.IndexKeyColumns(tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	_, err := statement.IndexKeyColumns("CREATE INDEX ON app.events (id)")
	assert.ErrorIs(t, err, statement.ErrIndexUnnamed)
}
