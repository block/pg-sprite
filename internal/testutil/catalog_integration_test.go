package testutil

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// catalogFixture covers the column, constraint, and index shapes the
// round-trip oracle compares: a serial and an identity column with
// non-default sequence parameters, a collated column, a NOT VALID check, a
// unique constraint, an expression index, and plain indexes over the serial
// column and over a column two indexes share, which give each of those
// columns several dependency edges besides its sequence's.
const catalogFixture = `CREATE TABLE t (
    id bigserial PRIMARY KEY,
    ref int GENERATED ALWAYS AS IDENTITY (START WITH 100 MAXVALUE 5000 CYCLE),
    "Label" text COLLATE "C" NOT NULL DEFAULT 'none',
    amount numeric(12,2),
    CONSTRAINT t_label_key UNIQUE ("Label")
);
ALTER TABLE t ADD CONSTRAINT t_amount_check CHECK (amount >= 0) NOT VALID;
CREATE INDEX t_lower_label_idx ON t (lower("Label"));
CREATE INDEX t_id_amount_idx ON t (id, amount);
CREATE INDEX t_amount_idx ON t (amount);`

// A table built from the same SQL in two schemas reads back the same
// catalog record, so the oracle compares tables across schemas. Each
// mutation afterwards changes one property the diff model does not carry,
// and the record differs in exactly that field.
func TestReadTableCatalog(t *testing.T) {
	url := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	left := NewSchema(t, pool)
	right := NewSchema(t, pool)
	ExecInSchema(t, pool, left, catalogFixture)
	ExecInSchema(t, pool, right, catalogFixture)

	want, err := ReadTableCatalog(t.Context(), pool, left, "t")
	require.NoError(t, err)
	got, err := ReadTableCatalog(t.Context(), pool, right, "t")
	require.NoError(t, err)
	require.Equal(t, want, got, "the same SQL in two schemas reads back the same record")

	columnNames := make([]string, 0, len(want.Columns))
	for _, c := range want.Columns {
		columnNames = append(columnNames, c.Name)
	}
	assert.Equal(t, "heap", want.AccessMethod)
	assert.Empty(t, want.Tablespace, "the database default tablespace")
	require.Equal(t, []string{"id", "ref", "Label", "amount"}, columnNames, "one record per column")
	assert.Equal(t, "nextval('t_id_seq'::regclass)", want.Columns[0].Default,
		"the default is decompiled without its schema")
	assert.Equal(t, &CatalogSequence{Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 9223372036854775807, Cache: 1}, want.Columns[0].Sequence)
	assert.Equal(t, "a", want.Columns[1].Identity)
	assert.Equal(t, &CatalogSequence{Type: "integer", Start: 100, Increment: 1, Min: 1, Max: 5000, Cache: 1, Cycle: true}, want.Columns[1].Sequence)
	assert.Equal(t, `"C"`, want.Columns[2].Collation)
	assert.Equal(t, []CatalogConstraint{
		{Name: "t_amount_check", Type: "c", Def: "CHECK ((amount >= (0)::numeric)) NOT VALID", Validated: false},
		{Name: "t_label_key", Type: "u", Def: `UNIQUE ("Label")`, Validated: true},
		{Name: "t_pkey", Type: "p", Def: "PRIMARY KEY (id)", Validated: true},
	}, withoutNotNullConstraints(want.Constraints))
	assert.Equal(t, []CatalogIndex{
		{Name: "t_amount_idx", Def: "CREATE INDEX t_amount_idx ON t USING btree (amount)", Valid: true},
		{Name: "t_id_amount_idx", Def: "CREATE INDEX t_id_amount_idx ON t USING btree (id, amount)", Valid: true},
		{Name: "t_label_key", Def: `CREATE UNIQUE INDEX t_label_key ON t USING btree ("Label")`, Valid: true},
		{Name: "t_lower_label_idx", Def: `CREATE INDEX t_lower_label_idx ON t USING btree (lower("Label"))`, Valid: true},
		{Name: "t_pkey", Def: "CREATE UNIQUE INDEX t_pkey ON t USING btree (id)", Valid: true},
	}, want.Indexes)

	t.Run("replica identity", func(t *testing.T) {
		ExecInSchema(t, pool, right, `ALTER TABLE t REPLICA IDENTITY FULL`)
		got := readCatalog(t, pool, right)
		assert.Equal(t, "d", want.ReplicaIdentity)
		assert.Equal(t, "f", got.ReplicaIdentity)
		ExecInSchema(t, pool, right, `ALTER TABLE t REPLICA IDENTITY DEFAULT`)
	})

	t.Run("storage parameter", func(t *testing.T) {
		ExecInSchema(t, pool, right, `ALTER TABLE t SET (fillfactor = 70)`)
		got := readCatalog(t, pool, right)
		assert.Empty(t, want.Options)
		assert.Equal(t, []string{"fillfactor=70"}, got.Options)
		ExecInSchema(t, pool, right, `ALTER TABLE t RESET (fillfactor)`)
	})

	t.Run("TOAST storage parameter", func(t *testing.T) {
		ExecInSchema(t, pool, right, `ALTER TABLE t SET (toast.autovacuum_enabled = false)`)
		got := readCatalog(t, pool, right)
		assert.Empty(t, want.ToastOptions)
		assert.Equal(t, []string{"autovacuum_enabled=false"}, got.ToastOptions)
		ExecInSchema(t, pool, right, `ALTER TABLE t RESET (toast.autovacuum_enabled)`)
	})

	t.Run("identity sequence parameter", func(t *testing.T) {
		ExecInSchema(t, pool, right, `ALTER TABLE t ALTER COLUMN ref SET MAXVALUE 9000`)
		got := readCatalog(t, pool, right)
		require.NotNil(t, got.Columns[1].Sequence)
		assert.Equal(t, int64(9000), got.Columns[1].Sequence.Max)
		ExecInSchema(t, pool, right, `ALTER TABLE t ALTER COLUMN ref SET MAXVALUE 5000`)
	})

	t.Run("constraint validation", func(t *testing.T) {
		ExecInSchema(t, pool, right, `ALTER TABLE t VALIDATE CONSTRAINT t_amount_check`)
		got := readCatalog(t, pool, right)
		assert.Equal(t, CatalogConstraint{Name: "t_amount_check", Type: "c", Def: "CHECK ((amount >= (0)::numeric))", Validated: true},
			withoutNotNullConstraints(got.Constraints)[0])
	})

	t.Run("column option", func(t *testing.T) {
		ExecInSchema(t, pool, right, `ALTER TABLE t ALTER COLUMN amount SET (n_distinct = 100)`)
		got := readCatalog(t, pool, right)
		assert.Empty(t, want.Columns[3].Options)
		assert.Equal(t, []string{"n_distinct=100"}, got.Columns[3].Options)
		ExecInSchema(t, pool, right, `ALTER TABLE t ALTER COLUMN amount RESET (n_distinct)`)
	})

	t.Run("statistics target", func(t *testing.T) {
		ExecInSchema(t, pool, right, `ALTER TABLE t ALTER COLUMN amount SET STATISTICS 500`)
		got := readCatalog(t, pool, right)
		assert.Equal(t, -1, want.Columns[3].StatisticsTarget)
		assert.Equal(t, 500, got.Columns[3].StatisticsTarget)
	})
}

func readCatalog(t *testing.T, pool *pgxpool.Pool, schema string) TableCatalog {
	t.Helper()
	tc, err := ReadTableCatalog(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	return tc
}

// withoutNotNullConstraints drops the NOT NULL constraints PostgreSQL 18
// records in pg_constraint, so the expected list is the same on every
// supported major.
func withoutNotNullConstraints(constraints []CatalogConstraint) []CatalogConstraint {
	var out []CatalogConstraint
	for _, c := range constraints {
		if c.Type != "n" {
			out = append(out, c)
		}
	}
	return out
}
