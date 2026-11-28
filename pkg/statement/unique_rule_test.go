package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// A unique key is read from both forms the catalog prints: a unique index's
// CREATE UNIQUE INDEX, and a unique constraint's or primary key's
// definition. INCLUDE columns are not part of the key.
func TestParseUniqueKeyReadsBothCatalogForms(t *testing.T) {
	ix, err := statement.ParseUniqueIndex(`CREATE UNIQUE INDEX orders_sku_uidx ON app.orders USING btree (sku COLLATE "C" text_pattern_ops, lower(region)) INCLUDE (id) NULLS NOT DISTINCT WHERE (archived IS FALSE)`)
	require.NoError(t, err)
	assert.Equal(t, statement.UniqueKey{
		Columns: []statement.UniqueKeyColumn{
			{Column: "sku", Collation: "C", OpClass: "text_pattern_ops"},
			{},
		},
		Method:           "btree",
		Predicate:        "SELECT WHERE archived IS FALSE",
		NullsNotDistinct: true,
	}, ix)

	con, err := statement.ParseUniqueConstraint("UNIQUE NULLS NOT DISTINCT (sku, region) INCLUDE (id) DEFERRABLE")
	require.NoError(t, err)
	assert.Equal(t, statement.UniqueKey{
		Columns:          []statement.UniqueKeyColumn{{Column: "sku"}, {Column: "region"}},
		Method:           "btree",
		NullsNotDistinct: true,
		Deferrable:       true,
	}, con)

	pk, err := statement.ParseUniqueConstraint("PRIMARY KEY (id)")
	require.NoError(t, err)
	assert.Equal(t, statement.UniqueKey{
		Columns: []statement.UniqueKeyColumn{{Column: "id"}},
		Method:  "btree",
		Primary: true,
	}, pk)
}

// A definition that enforces no uniqueness is an error, not an empty key.
func TestParseUniqueKeyRefusesOtherDefinitions(t *testing.T) {
	_, err := statement.ParseUniqueIndex("CREATE INDEX orders_sku_idx ON app.orders USING btree (sku)")
	require.ErrorIs(t, err, statement.ErrNotUniqueKey)
	_, err = statement.ParseUniqueConstraint("CHECK ((total > 0))")
	require.ErrorIs(t, err, statement.ErrNotUniqueKey)
}

// A key enforces a dropped one when every pair of rows the dropped key
// rejects, the key rejects too.
func TestUniqueKeyEnforces(t *testing.T) {
	index := func(t *testing.T, def string) statement.UniqueKey {
		t.Helper()
		key, err := statement.ParseUniqueIndex(def)
		require.NoError(t, err)
		return key
	}
	constraint := func(t *testing.T, def string) statement.UniqueKey {
		t.Helper()
		key, err := statement.ParseUniqueConstraint(def)
		require.NoError(t, err)
		return key
	}
	tests := []struct {
		name          string
		keeper, drops func(t *testing.T) statement.UniqueKey
		want          bool
	}{
		{
			"a constraint on the same column",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX u ON app.orders USING btree (sku)")
			},
			true,
		},
		{
			"an index with an INCLUDE column",
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX u ON app.orders USING btree (sku) INCLUDE (id)")
			},
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			true,
		},
		{
			"fewer columns",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku, region)") },
			true,
		},
		{
			"more columns",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku, region)") },
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			false,
		},
		{
			"a plain column of an expression key",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX u ON app.orders USING btree (sku, lower(region))")
			},
			true,
		},
		{
			"an expression",
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX u ON app.orders USING btree (lower(sku))")
			},
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			false,
		},
		{
			"another collation",
			func(t *testing.T) statement.UniqueKey {
				return index(t, `CREATE UNIQUE INDEX u ON app.orders USING btree (sku COLLATE "C")`)
			},
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			false,
		},
		{
			"no predicate over a partial key",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX u ON app.orders USING btree (sku) WHERE (archived IS FALSE)")
			},
			true,
		},
		{
			"a predicate over a full key",
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX u ON app.orders USING btree (sku) WHERE (archived IS FALSE)")
			},
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			false,
		},
		{
			"the same predicate",
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX v ON app.orders USING btree (sku) WHERE (archived IS FALSE)")
			},
			func(t *testing.T) statement.UniqueKey {
				return index(t, "CREATE UNIQUE INDEX u ON app.orders USING btree (sku, region) WHERE (archived IS FALSE)")
			},
			true,
		},
		{
			"NULLs distinct over NULLs not distinct",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE NULLS NOT DISTINCT (sku)") },
			false,
		},
		{
			"a primary key over NULLs not distinct",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "PRIMARY KEY (sku)") },
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE NULLS NOT DISTINCT (sku)") },
			true,
		},
		{
			"a deferrable key over an immediate one",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku) DEFERRABLE") },
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			false,
		},
		{
			"an immediate key over a deferrable one",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku)") },
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku) DEFERRABLE") },
			true,
		},
		{
			"an initially deferred key over an initially immediate one",
			func(t *testing.T) statement.UniqueKey {
				return constraint(t, "UNIQUE (sku) DEFERRABLE INITIALLY DEFERRED")
			},
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku) DEFERRABLE") },
			false,
		},
		{
			"an initially immediate key over an initially deferred one",
			func(t *testing.T) statement.UniqueKey { return constraint(t, "UNIQUE (sku) DEFERRABLE") },
			func(t *testing.T) statement.UniqueKey {
				return constraint(t, "UNIQUE (sku) DEFERRABLE INITIALLY DEFERRED")
			},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.keeper(t).Enforces(tt.drops(t)))
		})
	}
}

// The parser's grammar has no temporal key, so a WITHOUT OVERLAPS key is
// not read as a unique key, and no plain btree key is ever credited with
// keeping its overlap rule. A grammar that learns the clause fails this test,
// so the key's method and columns are modeled before it is read.
func TestParseUniqueConstraintRefusesATemporalKey(t *testing.T) {
	for _, def := range []string{
		"UNIQUE (id, valid_at WITHOUT OVERLAPS)",
		"PRIMARY KEY (id, valid_at WITHOUT OVERLAPS)",
	} {
		_, err := statement.ParseUniqueConstraint(def)
		assert.Error(t, err, def)
	}
}
