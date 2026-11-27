package executor

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The proof's inverse takes a validated CHECK off its table, and the
// attach that follows scans the table again. A second drop finds nothing
// and drops nothing.
func TestDropAttachProofTakesTheCheckOffAndIsIdempotent(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil, nil)
		require.NoError(t, err)

		require.NoError(t, dropAttachProof(t.Context(), f.pool, p, 0, nil))
		_, exists := f.proofValidated(t, p)
		assert.False(t, exists)
		require.NoError(t, dropAttachProof(t.Context(), f.pool, p, 0, nil), "nothing left to drop")

		assert.Contains(t, f.attach(t, attachJanuary), `verifying table "events_2027_01"`,
			"without its proof the attach scans the table")
	})
}

// A constraint that is not a CHECK under the proof's name is not the
// proof's: the drop refuses it and leaves it as it was.
func TestDropAttachProofRefusesANameHeldByAnotherConstraint(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		_, err = f.pool.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{f.schema, "events_2027_01"}.Sanitize()+
			" ADD CONSTRAINT "+pgx.Identifier{p.name()}.Sanitize()+" UNIQUE (id)")
		require.NoError(t, err)

		err = dropAttachProof(t.Context(), f.pool, p, 0, nil)
		require.ErrorIs(t, err, errAttachProofNameTaken)
		var contype string
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT contype::text FROM pg_catalog.pg_constraint
			WHERE conrelid = $1 AND conname = $2`, p.tableOID, p.name()).Scan(&contype))
		assert.Equal(t, "u", contype, "the unique constraint is left as it was")
	})
}
