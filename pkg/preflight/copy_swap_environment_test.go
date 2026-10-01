package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sufficientCopySwapEnvironment is a logical-decoding cluster with one free
// slot and sender and a volume holding exactly the required headroom for a
// 1 MiB table: every refusal below is one fact away from it.
func sufficientCopySwapEnvironment() (copySwapEnvironmentFacts, CopySwapEnvironment) {
	const tableBytes = 1 << 20
	facts := copySwapEnvironmentFacts{
		walLevel:            "logical",
		maxReplicationSlots: 4,
		usedSlots:           3,
		maxWALSenders:       4,
		usedWALSenders:      3,
		totalBytes:          tableBytes,
	}
	env := CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: copySwapDiskHeadroomFactor * tableBytes}
	return facts, env
}

// environmentCauseRaisedBy flips one fact of the sufficient environment per
// environment cause; the shape test consults it to account for the whole
// enumerated set.
var environmentCauseRaisedBy = map[CopySwapRefusalCause]func(f *copySwapEnvironmentFacts, env *CopySwapEnvironment){
	CopySwapCauseLogicalDecodingUnavailable: func(f *copySwapEnvironmentFacts, _ *CopySwapEnvironment) { f.walLevel = "replica" },
	CopySwapCauseSlotHeadroom:               func(f *copySwapEnvironmentFacts, _ *CopySwapEnvironment) { f.usedSlots = f.maxReplicationSlots },
	CopySwapCauseDiskHeadroom:               func(_ *copySwapEnvironmentFacts, env *CopySwapEnvironment) { env.FreeDiskBytes-- },
}

// Every environment cause is one the environment decision actually raises,
// and the sufficient environment raises none.
func TestRefuseCopySwapEnvironmentRaisesEveryEnvironmentCause(t *testing.T) {
	for cause, flip := range environmentCauseRaisedBy {
		facts, env := sufficientCopySwapEnvironment()
		flip(&facts, &env)
		refusal := refuseCopySwapEnvironment(facts, env)
		require.NotNil(t, refusal, cause)
		assert.Equal(t, cause, refusal.Cause)
		assert.NotEmpty(t, refusal.Detail)
	}

	facts, env := sufficientCopySwapEnvironment()
	assert.Nil(t, refuseCopySwapEnvironment(facts, env))
}

// The setting a logical-decoding refusal names follows the server: where the
// RDS parameter exists wal_level cannot be set directly, so the refusal
// names rds.logical_replication; on any other server, Cloud SQL and
// self-managed alike, it names wal_level itself.
func TestRefuseCopySwapEnvironmentNamesTheSettingBehindWALLevel(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	facts.walLevel = "replica"

	facts.rdsParameterPresent = false
	refusal := refuseCopySwapEnvironment(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseLogicalDecodingUnavailable, refusal.Cause)
	assert.Equal(t, CopySwapSettingWALLevel, refusal.Setting)

	facts.rdsParameterPresent = true
	refusal = refuseCopySwapEnvironment(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseLogicalDecodingUnavailable, refusal.Cause)
	assert.Equal(t, CopySwapSettingRDSLogicalReplication, refusal.Setting)
}

// The RDS parameter only decides which setting a decoding refusal names; a
// server that already has wal_level = logical is admitted whether or not
// the parameter exists.
func TestRefuseCopySwapEnvironmentIgnoresTheRDSParameterWhenDecodingIsEnabled(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	facts.rdsParameterPresent = true
	assert.Nil(t, refuseCopySwapEnvironment(facts, env))
}

// A WAL sender shortage is the same capacity cause as a slot shortage: the
// route needs one of each, and either missing stops it. The setting tells
// them apart, so an operator raises the limit that is actually exhausted.
func TestRefuseCopySwapEnvironmentCountsSendersAsSlotHeadroom(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	facts.usedWALSenders = facts.maxWALSenders
	refusal := refuseCopySwapEnvironment(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseSlotHeadroom, refusal.Cause)
	assert.Equal(t, CopySwapSettingMaxWALSenders, refusal.Setting)
	assert.NotEmpty(t, refusal.Detail)

	facts, env = sufficientCopySwapEnvironment()
	facts.usedSlots = facts.maxReplicationSlots
	refusal = refuseCopySwapEnvironment(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseSlotHeadroom, refusal.Cause)
	assert.Equal(t, CopySwapSettingMaxReplicationSlots, refusal.Setting)
}

// A quiesced run decodes nothing, so a replica-level cluster with no free
// slot is sufficient for it; disk headroom is required either way.
func TestRefuseCopySwapEnvironmentSkipsDecodingFactsWithoutLogicalDecoding(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	facts.walLevel = "replica"
	facts.usedSlots = facts.maxReplicationSlots
	env.LogicalDecoding = false
	assert.Nil(t, refuseCopySwapEnvironment(facts, env))

	env.FreeDiskBytes--
	refusal := refuseCopySwapEnvironment(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseDiskHeadroom, refusal.Cause)
}

// Free disk the caller did not measure is refused rather than assumed
// unlimited, and the boundary is inclusive: exactly the required bytes pass.
// No server setting governs disk, so a disk refusal names none.
func TestRefuseCopySwapEnvironmentRequiresMeasuredDisk(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	for _, unmeasured := range []int64{0, -1} {
		env.FreeDiskBytes = unmeasured
		refusal := refuseCopySwapEnvironment(facts, env)
		require.NotNil(t, refusal, unmeasured)
		assert.Equal(t, CopySwapCauseDiskHeadroom, refusal.Cause)
		assert.Empty(t, refusal.Setting)
		assert.NotEmpty(t, refusal.Detail)
	}

	env.FreeDiskBytes = facts.totalBytes * copySwapDiskHeadroomFactor
	require.Nil(t, refuseCopySwapEnvironment(facts, env))
}

// Enablement is decided before capacity and capacity before disk, so an
// operator fixes the setting that gates everything else first.
func TestRefuseCopySwapEnvironmentDecidesEnablementFirst(t *testing.T) {
	facts, env := sufficientCopySwapEnvironment()
	facts.walLevel = "minimal"
	facts.usedSlots = facts.maxReplicationSlots
	env.FreeDiskBytes = 0
	refusal := refuseCopySwapEnvironment(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseLogicalDecodingUnavailable, refusal.Cause)

	facts.walLevel = "logical"
	refusal = refuseCopySwapEnvironment(facts, env)
	require.NotNil(t, refusal)
	assert.Equal(t, CopySwapCauseSlotHeadroom, refusal.Cause)
}
