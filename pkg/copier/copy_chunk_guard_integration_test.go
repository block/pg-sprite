package copier_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// Every chunk transaction confirms the relations it is about to touch, not
// only the first one (ST-6): a shadow replaced by a same-shaped impostor
// after the resume clear passed and the first chunk was claimed, but before
// that chunk's transaction began — the gap the copier's first clock reading
// falls in — receives no rows, and the chunk stays in flight because it
// never landed.
func TestCopierRefusesAShadowReplacedAfterTheClaim(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 100)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()

	var replaceErr error
	clock := &hookedClock{hook: func() {
		if _, err := f.pool.Exec(t.Context(), "DROP TABLE "+shadowName); err != nil {
			replaceErr = fmt.Errorf("drop the shadow: %w", err)
			return
		}
		if _, err := f.pool.Exec(t.Context(), "CREATE TABLE "+shadowName+" (id bigint PRIMARY KEY, qty integer NOT NULL)"); err != nil {
			replaceErr = fmt.Errorf("create the impostor: %w", err)
		}
	}}
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1, Clock: clock})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.NoError(t, replaceErr, "the impostor was put in place between the claim and the chunk transaction")
	require.ErrorIs(t, err, copier.ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(ST-6): shadow")

	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"), "the impostor receives nothing")
	pos := c.Position()
	assert.Len(t, pos.InFlight, 1, "the refused chunk never landed")
	assert.Equal(t, int64(math.MinInt64), pos.InFlight[0].Lower())
	assert.False(t, pos.Watermark.Valid(), "nothing landed")
}

// Every chunk is written under the source owner's role, not the connected
// role (SET LOCAL ROLE owner): a shadow column defaulting to current_user
// records the owner on every copied row, so a shadow the builder made
// owner-correct stays owner-correct through the copy, and the copy has only
// the owner's privileges.
func TestCopierWritesAsTheTableOwner(t *testing.T) {
	f, owner := newOwnedCopierFixture(t)
	const rows = 100
	f.createOrders(t, rows)
	f.exec(t, "ALTER TABLE %s.orders OWNER TO "+pgx.Identifier{owner}.Sanitize())
	target := f.prove(t, "orders")
	require.Equal(t, owner, target.OwnerRole(), "the proof names the owner the copy runs as")
	lock := f.lock(t, "orders")
	alter, err := statement.ParseOne(fmt.Sprintf(`
		ALTER TABLE %s
			ADD COLUMN writer name NOT NULL DEFAULT current_user`, pgx.Identifier{f.schema, "orders"}.Sanitize()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	require.NoError(t, err)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 30, MaxRows: 30},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	var total, byOwner int64
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*), count(*) FILTER (WHERE writer = $1) FROM "+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize(),
		owner).Scan(&total, &byOwner))
	assert.Equal(t, int64(rows), total)
	assert.Equal(t, int64(rows), byOwner, "every copied row was written as the owner, not as the connected superuser")
}

// newOwnedCopierFixture is a copier fixture with a throwaway role that owns
// nothing yet and may create in the fixture schema, for tests whose source
// table is owned by a role other than the connected superuser. The role is
// created before the schema so the schema, and the grants on it, is dropped
// before the role is.
func newOwnedCopierFixture(t *testing.T) (copierFixture, string) {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	owner := testutil.NewRole(t, pool, "NOLOGIN")
	f := copierFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, "GRANT USAGE, CREATE ON SCHEMA %s TO "+pgx.Identifier{owner}.Sanitize())
	return f, owner
}
