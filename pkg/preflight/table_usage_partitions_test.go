package preflight

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idlePartitionedUsage is the evidence of a partitioned table whose three
// leaf partitions — two ranges and the default — were last scanned before
// the window opened, on the same server idleUsage describes. Each case
// changes one fact.
func idlePartitionedUsage() TableUsage {
	u := idleUsage()
	u.table = "events"
	u.partitioned = true
	u.leaves = []PartitionUsage{
		{schema: "app", name: "events_2025", oid: 2025, lastSeqScan: at(-3 * time.Hour), seqScans: 4, tuplesInserted: 10},
		{schema: "app", name: "events_2026", oid: 2026, lastSeqScan: at(-2 * time.Hour), lastIdxScan: at(-time.Hour), seqScans: 1, idxScans: 7, tuplesInserted: 5},
		{schema: "app", name: "events_default", oid: 2027, lastIdxScan: at(-4 * time.Hour), idxScans: 2},
	}
	return u
}

// requirePartitionVerdict judges u and checks the outcome, the reason, and
// the leaf partition the verdict names; an empty partition means none.
func requirePartitionVerdict(t *testing.T, u TableUsage, outcome UsageOutcome, reason UsageReason, partition string) {
	t.Helper()
	v, err := u.Judge(judgeWindow)
	require.NoError(t, err)
	assert.Equal(t, outcome, v.Outcome())
	assert.Equal(t, reason, v.Reason())
	p, named := v.Partition()
	if partition == "" {
		assert.False(t, named, "the verdict names no partition")
		return
	}
	require.True(t, named, "the verdict names partition %s", partition)
	assert.Equal(t, partition, p.Name())
}

func TestJudgePartitionedIdleWhenEveryLeafPredatesTheWindow(t *testing.T) {
	requirePartitionVerdict(t, idlePartitionedUsage(), UsageIdle, UsageReasonNoScanInWindow, "")
}

// The most recently scanned leaf decides and is named, not the first leaf
// with a scan inside the window.
func TestJudgePartitionedRecentNamesTheMostRecentlyScannedLeaf(t *testing.T) {
	u := idlePartitionedUsage()
	u.leaves[0].lastSeqScan = at(time.Hour)
	u.leaves[1].lastSeqScan = at(2 * time.Hour)
	requirePartitionVerdict(t, u, UsageRecent, UsageReasonSeqScanInWindow, "events_2026")
}

// The leaf named is the one scanned most recently by either kind of scan:
// an index scan of one leaf a minute ago beats a sequential scan of
// another two hours ago.
func TestJudgePartitionedNamesTheNewestScanOfEitherKind(t *testing.T) {
	u := idlePartitionedUsage()
	u.leaves[0].lastSeqScan = at(judgeWindow - 2*time.Hour)
	u.leaves[2].lastIdxScan = at(judgeWindow - time.Minute)
	requirePartitionVerdict(t, u, UsageRecent, UsageReasonIndexScanInWindow, "events_default")
}

// An index scan inside the window on any leaf, the default partition
// included, is use of the table.
func TestJudgePartitionedIndexScanOnTheDefaultPartitionIsRecent(t *testing.T) {
	u := idlePartitionedUsage()
	u.leaves[2].lastIdxScan = at(0)
	requirePartitionVerdict(t, u, UsageRecent, UsageReasonIndexScanInWindow, "events_default")
	u.leaves[2].lastIdxScan = at(-time.Microsecond)
	requirePartitionVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow, "")
}

// A lock on one partition is a lock on the table; the holder names the
// relation it locked.
func TestJudgePartitionedLockOnAPartitionIsRecent(t *testing.T) {
	u := idlePartitionedUsage()
	u.lockHolders = []LockHolder{{
		PID: 4242, Modes: []string{"RowExclusiveLock"},
		RelationSchema: "app", RelationName: "events_2026", RelationOID: 2026,
	}}
	requirePartitionVerdict(t, u, UsageRecent, UsageReasonLockHeld, "")
}

// A leaf with no recorded scan, while the server reports no reset time,
// cannot be shown idle on its own, and another leaf's old scan time does
// not bound when its counting began: the table is unknown, naming that
// leaf. A scan inside the window on another leaf is still use, and a reset
// time before the window makes "never scanned" idle.
func TestJudgePartitionedLeafNeverScannedWithoutResetTime(t *testing.T) {
	u := idlePartitionedUsage()
	u.statsReset = nil
	u.leaves[1].lastSeqScan, u.leaves[1].lastIdxScan = nil, nil
	requirePartitionVerdict(t, u, UsageUnknown, UsageReasonStatsStartUnknown, "events_2026")

	u.leaves[0].lastSeqScan = at(time.Minute)
	requirePartitionVerdict(t, u, UsageRecent, UsageReasonSeqScanInWindow, "events_2025")

	u.leaves[0].lastSeqScan = at(-3 * time.Hour)
	u.statsReset = at(-7 * 24 * time.Hour)
	requirePartitionVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow, "")
}

// A foreign-table partition keeps no scan statistics on this server, so
// the table cannot be shown idle; a scan of another leaf is still use.
func TestJudgePartitionedForeignLeaf(t *testing.T) {
	u := idlePartitionedUsage()
	u.leaves = append(u.leaves, PartitionUsage{schema: "app", name: "events_remote", oid: 2028, foreign: true})
	requirePartitionVerdict(t, u, UsageUnknown, UsageReasonForeignPartition, "events_remote")
	u.leaves[1].lastIdxScan = at(time.Hour)
	requirePartitionVerdict(t, u, UsageRecent, UsageReasonIndexScanInWindow, "events_2026")
}

// A partitioned table with no leaf partition holds no rows, and nothing
// records a query against it: unknown, unless a session holds a lock on
// it. Server-level reasons still come first.
func TestJudgePartitionedWithNoLeaf(t *testing.T) {
	u := idlePartitionedUsage()
	u.leaves = nil
	requirePartitionVerdict(t, u, UsageUnknown, UsageReasonNoPartitions, "")
	u.inRecovery = true
	requirePartitionVerdict(t, u, UsageUnknown, UsageReasonInRecovery, "")
	u.lockHolders = []LockHolder{{PID: 4242, Modes: []string{"AccessShareLock"}, RelationSchema: "app", RelationName: "events"}}
	requirePartitionVerdict(t, u, UsageRecent, UsageReasonLockHeld, "")
}

// Server-level reasons apply to a partitioned table as to an ordinary one:
// they come after evidence of use and before any partition's own reason.
func TestJudgePartitionedServerReasonsComeBeforePartitionReasons(t *testing.T) {
	u := idlePartitionedUsage()
	u.leaves = append(u.leaves, PartitionUsage{schema: "app", name: "events_remote", foreign: true})
	u.statsReset = at(time.Hour)
	requirePartitionVerdict(t, u, UsageUnknown, UsageReasonStatsResetInWindow, "")
	u.serverVersionNum = 150008
	requirePartitionVerdict(t, u, UsageUnknown, UsageReasonScanTimesNotRecorded, "")
}

// The table's scan times are the latest across its leaves and its counters
// the sums; an ordinary table lists no partitions.
func TestTableUsageAggregatesItsLeaves(t *testing.T) {
	u := idlePartitionedUsage()
	lastSeq, ok := u.LastSeqScan()
	require.True(t, ok)
	assert.Equal(t, *at(-2 * time.Hour), lastSeq)
	lastIdx, ok := u.LastIdxScan()
	require.True(t, ok)
	assert.Equal(t, *at(-time.Hour), lastIdx)
	assert.Equal(t, int64(5), u.SeqScans())
	assert.Equal(t, int64(9), u.IdxScans())
	assert.Equal(t, int64(15), u.TuplesInserted())
	assert.Equal(t, []string{"events_2025", "events_2026", "events_default"}, partitionNames(u.Partitions()))

	assert.Empty(t, idleUsage().Partitions())
	none := idlePartitionedUsage()
	none.leaves = nil
	_, ok = none.LastSeqScan()
	assert.False(t, ok)
	assert.Zero(t, none.SeqScans())
}

func partitionNames(partitions []PartitionUsage) []string {
	names := make([]string, 0, len(partitions))
	for _, p := range partitions {
		names = append(names, p.Name())
	}
	return names
}
