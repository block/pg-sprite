package preflight

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// judgeWindow is the window every unit case is judged against, and
// judgeReadAt the server time of the read, so the window opens at
// judgeReadAt minus a day.
const judgeWindow = 24 * time.Hour

var judgeReadAt = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// at returns a pointer to the instant offset from the window's start: a
// negative offset is before the window, zero is its edge.
func at(offset time.Duration) *time.Time {
	t := judgeReadAt.Add(-judgeWindow).Add(offset)
	return &t
}

// idleUsage is the evidence of a table whose last scans of both kinds
// were an hour before the window opened, on a PostgreSQL 16 primary that
// tracks counts and whose statistics were last reset a week before the
// window. Each case changes one fact.
func idleUsage() TableUsage {
	return TableUsage{
		schema:           "app",
		table:            "orders_archive",
		serverVersionNum: 160004,
		trackCounts:      true,
		readAt:           judgeReadAt,
		statsReset:       at(-7 * 24 * time.Hour),
		leaves: []PartitionUsage{{
			schema:      "app",
			name:        "orders_archive",
			lastSeqScan: at(-time.Hour),
			lastIdxScan: at(-time.Hour),
		}},
	}
}

func requireVerdict(t *testing.T, u TableUsage, outcome UsageOutcome, reason UsageReason) {
	t.Helper()
	v, err := u.Judge(judgeWindow)
	require.NoError(t, err)
	assert.Equal(t, outcome, v.Outcome())
	assert.Equal(t, reason, v.Reason())
	assert.Equal(t, judgeWindow, v.Window())
	assert.Equal(t, u.readAt.Add(-judgeWindow), v.WindowStart())
	assert.Equal(t, u.readAt, v.Usage().ReadAt())
	_, named := v.Partition()
	assert.False(t, named, "an ordinary table's verdict names no partition")
}

func TestJudgeIdleWhenEveryScanPredatesTheWindow(t *testing.T) {
	requireVerdict(t, idleUsage(), UsageIdle, UsageReasonNoScanInWindow)
}

// A scan at the window's first instant is inside it; one a microsecond
// earlier is not. Both edges are pinned for both kinds of scan.
func TestJudgeSeqScanAtTheWindowEdge(t *testing.T) {
	u := idleUsage()
	u.leaves[0].lastSeqScan = at(0)
	requireVerdict(t, u, UsageRecent, UsageReasonSeqScanInWindow)
	u.leaves[0].lastSeqScan = at(-time.Microsecond)
	requireVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow)
}

func TestJudgeIndexScanAtTheWindowEdge(t *testing.T) {
	u := idleUsage()
	u.leaves[0].lastIdxScan = at(0)
	requireVerdict(t, u, UsageRecent, UsageReasonIndexScanInWindow)
	u.leaves[0].lastIdxScan = at(-time.Microsecond)
	requireVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow)
}

// A scan recorded after the read's own server time — a transaction that
// committed after the read began and published before the counters were
// read — is inside the window too.
func TestJudgeScanAfterTheReadIsRecent(t *testing.T) {
	u := idleUsage()
	u.leaves[0].lastSeqScan = at(judgeWindow + time.Second)
	requireVerdict(t, u, UsageRecent, UsageReasonSeqScanInWindow)
}

// A session holding a lock is use, even when every scan is old and even
// on a server that records no scan times.
func TestJudgeLockHolderIsRecent(t *testing.T) {
	u := idleUsage()
	u.lockHolders = []LockHolder{{PID: 4242, ApplicationName: "reporting", Modes: []string{"AccessShareLock"}}}
	requireVerdict(t, u, UsageRecent, UsageReasonLockHeld)
	u.serverVersionNum = 140010
	requireVerdict(t, u, UsageRecent, UsageReasonLockHeld)
}

// PostgreSQL 14 and 15 keep no scan times, so nothing can be proven idle
// there; 16.0 is the first version that can.
func TestJudgeUnknownWithoutScanTimes(t *testing.T) {
	u := idleUsage()
	u.leaves[0].lastSeqScan, u.leaves[0].lastIdxScan = nil, nil
	u.serverVersionNum = 150008
	requireVerdict(t, u, UsageUnknown, UsageReasonScanTimesNotRecorded)
	u.serverVersionNum = 160000
	requireVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow)
}

// A standby's statistics describe the standby: old scans there prove
// nothing about the primary, but a recent one is still use.
func TestJudgeInRecovery(t *testing.T) {
	u := idleUsage()
	u.inRecovery = true
	requireVerdict(t, u, UsageUnknown, UsageReasonInRecovery)
	u.leaves[0].lastIdxScan = at(time.Hour)
	requireVerdict(t, u, UsageRecent, UsageReasonIndexScanInWindow)
}

func TestJudgeUnknownWhenCountsAreNotTracked(t *testing.T) {
	u := idleUsage()
	u.trackCounts = false
	requireVerdict(t, u, UsageUnknown, UsageReasonCountsNotTracked)
}

// A reset at the window's first instant is inside it; one a microsecond
// earlier is not.
func TestJudgeStatsResetAtTheWindowEdge(t *testing.T) {
	u := idleUsage()
	u.statsReset = at(0)
	requireVerdict(t, u, UsageUnknown, UsageReasonStatsResetInWindow)
	u.statsReset = at(-time.Microsecond)
	requireVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow)
}

// A single-table reset clears the table's sequential scan time but leaves
// its index's scan time standing, and moves the database's reset time: an
// old index scan time does not make the reset harmless.
func TestJudgeResetInWindowOverridesAnOldScanTime(t *testing.T) {
	u := idleUsage()
	u.leaves[0].lastSeqScan = nil
	u.statsReset = at(time.Hour)
	requireVerdict(t, u, UsageUnknown, UsageReasonStatsResetInWindow)
}

// "Never scanned" proves idleness only when counting began before the
// window: a reset time before it says so; no reset time — fresh statistics
// or statistics discarded by crash recovery — does not.
func TestJudgeNeverScanned(t *testing.T) {
	u := idleUsage()
	u.leaves[0].lastSeqScan, u.leaves[0].lastIdxScan = nil, nil
	requireVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow)
	u.statsReset = nil
	requireVerdict(t, u, UsageUnknown, UsageReasonStatsStartUnknown)
}

// A recorded scan time bounds when counting began, so with one old scan
// time and no reset time the other kind's absence covers the window.
func TestJudgeOneOldScanTimeWithoutResetTimeIsIdle(t *testing.T) {
	u := idleUsage()
	u.statsReset = nil
	u.leaves[0].lastSeqScan = nil
	requireVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow)
	u.leaves[0].lastSeqScan, u.leaves[0].lastIdxScan = at(-time.Hour), nil
	requireVerdict(t, u, UsageIdle, UsageReasonNoScanInWindow)
}

func TestJudgeZeroUsageIsNotIdle(t *testing.T) {
	requireVerdict(t, TableUsage{}, UsageUnknown, UsageReasonScanTimesNotRecorded)
}

func TestJudgeRefusesAWindowBelowTheFloor(t *testing.T) {
	_, err := idleUsage().Judge(MinUsageWindow - time.Nanosecond)
	var windowErr *UsageWindowError
	require.ErrorAs(t, err, &windowErr)
	assert.Equal(t, MinUsageWindow-time.Nanosecond, windowErr.Window)
	assert.Equal(t, MinUsageWindow, windowErr.Min)

	v, err := idleUsage().Judge(MinUsageWindow)
	require.NoError(t, err)
	assert.Equal(t, UsageIdle, v.Outcome())
}
