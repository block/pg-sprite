package dbconn

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostDefaultPool is the pool configuration pgxpool.ParseConfig builds from
// url on a host whose default pool size is defaultMaxConns. pgxpool takes
// its default from the host's CPU count, so the tests pin it to the value a
// small host gets instead of reading whatever this host has.
func hostDefaultPool(t *testing.T, url string, defaultMaxConns int32) *pgxpool.Config {
	t.Helper()
	pc, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	pc.MaxConns = defaultMaxConns
	return pc
}

// A URL with no pool_max_conns leaves the size at pgxpool's default, which
// is four connections on a host with four CPUs or fewer; the floor raises
// it to six.
func TestSizePoolRaisesTheDefaultToTheFloor(t *testing.T) {
	url := testURL
	pc := hostDefaultPool(t, url, 4)

	require.NoError(t, sizePool(pc, Config{URL: url, MaxConnsFloor: 6}))
	assert.Equal(t, int32(6), pc.MaxConns)
}

// A default already above the floor is kept: the floor only ever raises.
func TestSizePoolKeepsADefaultAboveTheFloor(t *testing.T) {
	url := testURL
	pc := hostDefaultPool(t, url, 16)

	require.NoError(t, sizePool(pc, Config{URL: url, MaxConnsFloor: 6}))
	assert.Equal(t, int32(16), pc.MaxConns)
}

// pool_max_conns in the URL is the operator's choice and is kept even below
// the floor, in both connection-string forms; the work that needs more
// connections refuses the pool instead.
func TestSizePoolKeepsTheURLsPoolMaxConns(t *testing.T) {
	for _, url := range []string{
		"postgres://engine@localhost:5432/app?pool_max_conns=4",
		"host=localhost port=5432 user=engine dbname=app pool_max_conns=4",
	} {
		t.Run(url, func(t *testing.T) {
			pc, err := pgxpool.ParseConfig(url)
			require.NoError(t, err)
			require.Equal(t, int32(4), pc.MaxConns)

			require.NoError(t, sizePool(pc, Config{URL: url, MaxConnsFloor: 6}))
			assert.Equal(t, int32(4), pc.MaxConns)
		})
	}
}

// Config.MaxConns is the library caller's choice and wins over the URL and
// the floor alike.
func TestSizePoolAppliesMaxConns(t *testing.T) {
	const url = "postgres://engine@localhost:5432/app?pool_max_conns=10"
	pc, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)

	require.NoError(t, sizePool(pc, Config{URL: url, MaxConns: 3, MaxConnsFloor: 6}))
	assert.Equal(t, int32(3), pc.MaxConns)
}

// buildPoolConfig wires the floor through: with a floor above whatever this
// host's default is, a URL without pool_max_conns gets the floor.
func TestBuildPoolConfigAppliesMaxConnsFloor(t *testing.T) {
	url := testURL
	parsed, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	floor := parsed.MaxConns + 2

	pc, err := buildPoolConfig(Config{URL: url, MaxConnsFloor: floor})
	require.NoError(t, err)
	assert.Equal(t, floor, pc.MaxConns)
}
