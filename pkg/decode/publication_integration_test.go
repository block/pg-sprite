package decode_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// A publication of the derived name that publishes anything beyond the
// target — another table alongside it, or every table — is not the route's
// and is refused rather than adopted, with no slot created behind it.
func TestCreateSlotRefusesAPublicationThatIsNotExactlyTheTarget(t *testing.T) {
	f := newSlotFixture(t)
	name := f.target.DecodingName()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.sibling (id bigint PRIMARY KEY)`, f.schema))
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.ledger, %s.sibling`, name, f.schema, f.schema))
	require.NoError(t, err)
	_, err = decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.ErrorIs(t, err, decode.ErrForeignDecodingState)
	var foreign *decode.ForeignStateError
	require.ErrorAs(t, err, &foreign)
	assert.Equal(t, decode.ForeignObjectPublication, foreign.Object)
	assert.Equal(t, name, foreign.Name)
	_, found, err := decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.False(t, found)
	assert.True(t, f.publicationExists(t, name), "the foreign publication is left as found")
	_, err = f.pool.Exec(t.Context(), `DROP PUBLICATION `+name)
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), `CREATE PUBLICATION `+name+` FOR ALL TABLES`)
	require.NoError(t, err)
	_, err = decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.ErrorIs(t, err, decode.ErrForeignDecodingState)
	_, found, err = decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.False(t, found)
	_, err = f.pool.Exec(t.Context(), `DROP PUBLICATION `+name)
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.ledger`, name, f.schema))
	require.NoError(t, err)
	slot := f.createSlot(t)
	assert.Equal(t, []string{f.schema + ".ledger"}, f.publishedTables(t, slot.Name()),
		"a publication of exactly the target is the route's own and is reused")
}

// A publication of the derived name that publishes one other table — and
// so exactly one table, like the route's own — is not the route's: the
// table is checked, not just the count.
func TestCreateSlotRefusesAPublicationOfAnotherTable(t *testing.T) {
	f := newSlotFixture(t)
	name := f.target.DecodingName()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.sibling (id bigint PRIMARY KEY)`, f.schema))
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.sibling`, name, f.schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `DROP PUBLICATION IF EXISTS `+name)
		assert.NoError(t, err)
	})

	_, err = decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	var foreign *decode.ForeignStateError
	require.ErrorAs(t, err, &foreign)
	assert.Equal(t, decode.ForeignObjectPublication, foreign.Object)
	assert.Contains(t, foreign.Detail, "not the target")
	_, found, err := decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.False(t, found)
	assert.True(t, f.publicationExists(t, name), "the other table's publication is left as found")
}

// A publication of the derived name that publishes the target but leaves
// some of its changes out of the stream — an operation, a row, a column —
// is refused rather than adopted: a slot behind it would miss changes the
// swap then loses. Each shape is created as an operator might, and each is
// refused for the reason the catalog shows. A row filter, a column list,
// and a whole-schema membership exist only from PostgreSQL 15.
func TestCreateSlotRefusesAPublicationThatPublishesLessThanEveryChange(t *testing.T) {
	f := newSlotFixture(t)
	name := f.target.DecodingName()
	var version int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version))
	const publicationFiltersSince = 150000

	for _, c := range []struct {
		shape  string
		create string
		since  int
	}{
		{
			shape:  "inserts only",
			create: fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.ledger WITH (publish = 'insert')`, name, f.schema),
		},
		{
			shape:  "no truncate",
			create: fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.ledger WITH (publish = 'insert, update, delete')`, name, f.schema),
		},
		{
			shape:  "row filter",
			create: fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.ledger WHERE (id > 50)`, name, f.schema),
			since:  publicationFiltersSince,
		},
		{
			shape:  "column list",
			create: fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s.ledger (id)`, name, f.schema),
			since:  publicationFiltersSince,
		},
		{
			shape:  "whole schema",
			create: fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLES IN SCHEMA %s`, name, f.schema),
			since:  publicationFiltersSince,
		},
	} {
		t.Run(c.shape, func(t *testing.T) {
			if version < c.since {
				t.Skip("this publication shape is not expressible on this server")
			}
			_, err := f.pool.Exec(t.Context(), c.create)
			require.NoError(t, err, c.create)
			t.Cleanup(func() {
				_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `DROP PUBLICATION IF EXISTS `+name)
				assert.NoError(t, err)
			})

			_, err = decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
			var foreign *decode.ForeignStateError
			require.ErrorAs(t, err, &foreign)
			assert.Equal(t, decode.ForeignObjectPublication, foreign.Object)
			_, found, err := decode.InspectSlot(t.Context(), f.pool, name)
			require.NoError(t, err)
			assert.False(t, found, "no slot is created behind a publication that publishes less")
			assert.True(t, f.publicationExists(t, name), "the narrower publication is left as found")
		})
	}
}
