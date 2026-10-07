package decode_test

import (
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
