package applier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// pathShadow is the least shadow NewFlusher accepts, minted by hand so the
// path a flusher picks can be read off it; flushes into real tables use the
// shadow builder's proof.
type pathShadow struct {
	schema      string
	conversions map[string]string
}

func (s pathShadow) Schema() string                 { return s.schema }
func (s pathShadow) SourceTable() string            { return "orders" }
func (s pathShadow) ShadowTable() string            { return "_pgsprite_orders_new" }
func (s pathShadow) SourceOID() uint32              { return 1 }
func (s pathShadow) ShadowOID() uint32              { return 2 }
func (s pathShadow) CopyColumns() []string          { return []string{"id", "qty"} }
func (s pathShadow) Conversions() map[string]string { return s.conversions }

// The path every flush sets is decided by whether the shadow proof carries
// conversions, and by nothing else: without them the catalog alone, so a
// schema ahead of it on the caller's pool cannot answer a shadow default
// or trigger; with them the shadow build's own path, so the expressions
// resolve as they did when the gated statement ran. Both paths lead with
// the catalog, so no decoy tells them apart at the server; the pin is the
// path itself (CO-9).
func TestNewFlusherWidensTheSearchPathOnlyForConversions(t *testing.T) {
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), `CREATE TABLE `+schema+`.orders (id bigint PRIMARY KEY, qty text)`)
	require.NoError(t, err)
	target, err := preflight.CheckCopySwap(t.Context(), pool, schema, "orders", preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	lock, err := dbconn.AcquireTableLock(t.Context(), cfg, schema, "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, lock.Release(context.WithoutCancel(t.Context()))) })

	bare, err := NewFlusher(target, pathShadow{schema: schema}, lock, Options{})
	require.NoError(t, err)
	converted, err := NewFlusher(target, pathShadow{schema: schema, conversions: map[string]string{"qty": "qty::integer"}}, lock, Options{})
	require.NoError(t, err)

	assert.Equal(t, dbconn.LocalSearchPath("pg_catalog"), bare.searchPath, "a conversion-free flush keeps the catalog alone")
	assert.Equal(t, dbconn.LocalSearchPath(schema, "public"), converted.searchPath, "a converted flush runs under the shadow build's path")
}
