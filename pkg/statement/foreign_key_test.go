package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A desired file may declare foreign keys in both grammar forms. The parse
// lists the tables they reference so the scratch build can stand each one
// in before the CREATE TABLE runs.
func TestParseDesiredAdmitsForeignKeys(t *testing.T) {
	ds, err := ParseDesired(`
		CREATE TABLE orders (
			id bigint PRIMARY KEY,
			user_id bigint REFERENCES users (id),
			store_id bigint,
			FOREIGN KEY (store_id) REFERENCES stores (id) ON DELETE CASCADE
		)`)
	require.NoError(t, err)
	assert.Equal(t, "orders", ds.Table())
	assert.True(t, ds.HasForeignKeys())
	assert.Equal(t, []string{"stores", "users"}, ds.ReferencedTables())
}

// Two foreign keys into one table need one stand-in, so the referenced table
// is listed once.
func TestParseDesiredListsEachReferencedTableOnce(t *testing.T) {
	ds, err := ParseDesired(`
		CREATE TABLE transfers (
			id bigint PRIMARY KEY,
			from_account bigint REFERENCES accounts (id),
			to_account bigint REFERENCES accounts (id)
		)`)
	require.NoError(t, err)
	assert.Equal(t, []string{"accounts"}, ds.ReferencedTables())
}

// A self-reference binds to the table the desired file itself creates, so it
// needs no stand-in; the file still declares a foreign key.
func TestParseDesiredSelfReferenceNeedsNoStandIn(t *testing.T) {
	ds, err := ParseDesired(`
		CREATE TABLE node (
			id bigint PRIMARY KEY,
			parent_id bigint REFERENCES node (id)
		)`)
	require.NoError(t, err)
	assert.True(t, ds.HasForeignKeys())
	assert.Empty(t, ds.ReferencedTables())
}

// A table without foreign keys references nothing.
func TestParseDesiredWithoutForeignKeys(t *testing.T) {
	ds, err := ParseDesired(`
		CREATE TABLE users (
			id bigint PRIMARY KEY
		)`)
	require.NoError(t, err)
	assert.False(t, ds.HasForeignKeys())
	assert.Empty(t, ds.ReferencedTables())
}

// The row-security scratch build creates no stand-ins, so a foreign key there
// would bind to the live referenced table; the row-security door refuses it.
func TestParseDesiredWithRowSecurityRefusesForeignKey(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`
		CREATE TABLE documents (
			id bigint PRIMARY KEY,
			owner_id bigint REFERENCES users (id)
		);
		ALTER TABLE documents ENABLE ROW LEVEL SECURITY`)
	require.ErrorIs(t, err, ErrRowSecurityForeignKey)
}

// The create path reads Op.ForeignKey to refuse a new table that declares a
// foreign key; a table without one carries no flag.
func TestParseOpsMarksCreateTableForeignKey(t *testing.T) {
	ops, err := ParseOps(`
		CREATE TABLE public.orders (
			id bigint PRIMARY KEY,
			user_id bigint REFERENCES users (id)
		)`)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.True(t, ops[0].ForeignKey)

	ops, err = ParseOps(`
		CREATE TABLE public.users (
			id bigint PRIMARY KEY
		)`)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.False(t, ops[0].ForeignKey)
}

// A planned foreign-key add names its referenced table in the target schema,
// so the statement binds the same table whatever search_path runs it. A
// reference the statement already qualifies is kept, and a constraint that
// is not a foreign key is untouched.
func TestQualifyForeignKeyReferences(t *testing.T) {
	cases := []struct {
		name, sql, want string
	}{
		{
			"unqualified reference takes the target schema",
			`ALTER TABLE app.orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id)`,
			`ALTER TABLE app.orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES app.users (id)`,
		},
		{
			"qualified reference is kept",
			`ALTER TABLE app.orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES other.users(id)`,
			`ALTER TABLE app.orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES other.users (id)`,
		},
		{
			"check constraint is untouched",
			`ALTER TABLE app.orders ADD CONSTRAINT orders_qty_check CHECK (qty > 0)`,
			`ALTER TABLE app.orders ADD CONSTRAINT orders_qty_check CHECK (qty > 0)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := QualifyForeignKeyReferences(tc.sql, "app")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := QualifyForeignKeyReferences(`CREATE INDEX i ON t (a)`, "app")
	require.ErrorIs(t, err, ErrDisallowedStatement)
}
