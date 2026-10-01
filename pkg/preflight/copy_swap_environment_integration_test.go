package preflight_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// copySwapEnvironmentFixture is a schema holding one admitted table and the
// CopySwapTarget proof minted for it, on whichever server the test needs:
// the shared harness for disk headroom, a dedicated container for the
// restart-only settings.
type copySwapEnvironmentFixture struct {
	pool   *pgxpool.Pool
	schema string
	target preflight.CopySwapTarget
}

// newCopySwapEnvironmentFixture creates a bigint-keyed table with enough
// rows to occupy heap pages, so the disk requirement is a real size rather
// than an empty relation's, and mints its copy-and-swap proof.
func newCopySwapEnvironmentFixture(t *testing.T, serverURL string) copySwapEnvironmentFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.ledger (
			id bigint PRIMARY KEY,
			note text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.ledger (id, note)
		SELECT g, repeat('x', 100) FROM generate_series(1, 1000) AS g`, schema))
	require.NoError(t, err)

	role, err := preflight.CheckPrivileges(t.Context(), pool, schema, "ledger", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), pool, schema, "ledger", role)
	require.NoError(t, err)
	return copySwapEnvironmentFixture{pool: pool, schema: schema, target: target}
}

// totalBytes is the table's heap, indexes, and TOAST as the server
// measures them — the independent oracle for the disk requirement.
func (f copySwapEnvironmentFixture) totalBytes(t *testing.T) int64 {
	t.Helper()
	var total int64
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT pg_total_relation_size($1::regclass)`, f.schema+".ledger").Scan(&total))
	require.Positive(t, total)
	return total
}

func (f copySwapEnvironmentFixture) check(t *testing.T, env preflight.CopySwapEnvironment) error {
	t.Helper()
	return preflight.CheckCopySwapEnvironment(t.Context(), f.pool, f.target, env)
}

// requireCopySwapEnvironmentCause asserts the refusal's cause and the
// setting it asks the operator to change; the zero setting is a refusal
// that no server setting resolves.
func requireCopySwapEnvironmentCause(t *testing.T, err error, want preflight.CopySwapRefusalCause, setting preflight.CopySwapSetting) {
	t.Helper()
	var envErr *preflight.CopySwapEnvironmentError
	require.ErrorAs(t, err, &envErr)
	assert.Equal(t, want, envErr.Cause)
	assert.Equal(t, setting, envErr.Setting)
	assert.NotEmpty(t, envErr.Detail)
	assert.Equal(t, want, preflight.CopySwapRefusalCauseOf(err))
}

// unlimitedDisk stands in for a measured volume with more free space than
// any fixture table needs, so a test isolates the decoding facts.
const unlimitedDisk = math.MaxInt64

// A cluster at wal_level = replica cannot host a logical slot, so a run
// that decodes WAL is refused before anything is written and told to set
// wal_level itself; a quiesced run decodes nothing and is admitted on the
// same cluster.
func TestCheckCopySwapEnvironmentRefusesWithoutLogicalDecoding(t *testing.T) {
	f := newCopySwapEnvironmentFixture(t, testutil.StartPostgresWithSettings(t, "wal_level=replica"))

	err := f.check(t, preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: unlimitedDisk})
	requireCopySwapEnvironmentCause(t, err, preflight.CopySwapCauseLogicalDecodingUnavailable, preflight.CopySwapSettingWALLevel)

	err = f.check(t, preflight.CopySwapEnvironment{LogicalDecoding: false, FreeDiskBytes: unlimitedDisk})
	require.NoError(t, err, "a quiesced run has no use for logical decoding")
}

// Where the server carries the rds.logical_replication parameter, wal_level
// cannot be set directly, so the refusal names that parameter instead. A
// plain server defines the parameter the same way RDS does — as a setting
// with a dotted name — so the detection sees exactly what it would see on
// RDS, with wal_level itself still reported as the measured level.
func TestCheckCopySwapEnvironmentNamesTheRDSParameterWhereItExists(t *testing.T) {
	f := newCopySwapEnvironmentFixture(t, testutil.StartPostgresWithSettings(t,
		"wal_level=replica", "rds.logical_replication=0"))

	err := f.check(t, preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: unlimitedDisk})
	requireCopySwapEnvironmentCause(t, err, preflight.CopySwapCauseLogicalDecodingUnavailable, preflight.CopySwapSettingRDSLogicalReplication)
}

// A logical cluster with one free slot and one free WAL sender admits a
// decoding run; once another consumer holds the last slot the same run is
// refused for capacity, and dropping that slot admits it again.
func TestCheckCopySwapEnvironmentRequiresAFreeReplicationSlot(t *testing.T) {
	f := newCopySwapEnvironmentFixture(t, testutil.StartPostgresWithSettings(t,
		"wal_level=logical", "max_replication_slots=1", "max_wal_senders=1"))
	decoding := preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: unlimitedDisk}

	require.NoError(t, f.check(t, decoding))

	_, err := f.pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot('other_consumer', 'pgoutput')`)
	require.NoError(t, err)
	err = f.check(t, decoding)
	requireCopySwapEnvironmentCause(t, err, preflight.CopySwapCauseSlotHeadroom, preflight.CopySwapSettingMaxReplicationSlots)

	_, err = f.pool.Exec(t.Context(), `SELECT pg_drop_replication_slot('other_consumer')`)
	require.NoError(t, err)
	require.NoError(t, f.check(t, decoding))
}

// A logical cluster with a free slot but max_wal_senders = 0 cannot accept
// the replication connection that would consume the slot, so capacity is
// refused on the sender side alone.
func TestCheckCopySwapEnvironmentRequiresAFreeWALSender(t *testing.T) {
	f := newCopySwapEnvironmentFixture(t, testutil.StartPostgresWithSettings(t,
		"wal_level=logical", "max_replication_slots=1", "max_wal_senders=0"))

	err := f.check(t, preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: unlimitedDisk})
	requireCopySwapEnvironmentCause(t, err, preflight.CopySwapCauseSlotHeadroom, preflight.CopySwapSettingMaxWALSenders)
}

// The disk requirement is a multiple of the table's measured total size:
// exactly that much free space is admitted, one byte less is refused, and
// a caller that measured nothing is refused rather than waved through.
func TestCheckCopySwapEnvironmentRequiresDiskHeadroom(t *testing.T) {
	f := newCopySwapEnvironmentFixture(t, testutil.StartPostgres(t))
	required := 2 * f.totalBytes(t)

	require.NoError(t, f.check(t, preflight.CopySwapEnvironment{FreeDiskBytes: required}))

	err := f.check(t, preflight.CopySwapEnvironment{FreeDiskBytes: required - 1})
	requireCopySwapEnvironmentCause(t, err, preflight.CopySwapCauseDiskHeadroom, "")

	err = f.check(t, preflight.CopySwapEnvironment{FreeDiskBytes: 0})
	requireCopySwapEnvironmentCause(t, err, preflight.CopySwapCauseDiskHeadroom, "")
}

// The check measures the proven relation, so a forged zero proof and a
// proof whose relation has since been dropped are both proof mismatches,
// never environment refusals.
func TestCheckCopySwapEnvironmentRejectsUnprovenTargets(t *testing.T) {
	f := newCopySwapEnvironmentFixture(t, testutil.StartPostgres(t))
	env := preflight.CopySwapEnvironment{FreeDiskBytes: unlimitedDisk}

	err := preflight.CheckCopySwapEnvironment(t.Context(), f.pool, preflight.CopySwapTarget{}, env)
	require.ErrorIs(t, err, preflight.ErrCopySwapProofMismatch)

	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`DROP TABLE %s.ledger`, f.schema))
	require.NoError(t, err)
	err = f.check(t, env)
	require.ErrorIs(t, err, preflight.ErrCopySwapProofMismatch)
	assert.Empty(t, preflight.CopySwapRefusalCauseOf(err))
}
