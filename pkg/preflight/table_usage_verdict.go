package preflight

import (
	"fmt"
	"time"
)

// MinUsageWindow is the shortest window Judge accepts. A scan is recorded
// with the end time of the transaction that ran it, but the backend
// publishes it to the shared statistics only when it next goes idle, at
// most once a second, and defers that by up to a minute when the
// statistics are contended. A window not well clear of that delay would
// answer about scans the server has not reported yet.
const MinUsageWindow = 5 * time.Minute

// UsageWindowError reports a window shorter than MinUsageWindow.
type UsageWindowError struct {
	// Window is the window asked for.
	Window time.Duration
	// Min is the shortest window Judge accepts.
	Min time.Duration
}

// Error implements the error interface.
func (e *UsageWindowError) Error() string {
	return fmt.Sprintf("usage window %s is shorter than the %s the server's statistics need to publish a finished scan", e.Window, e.Min)
}

// UsageOutcome is the closed set of answers to "was this table used inside
// the window, on this server?".
type UsageOutcome string

const (
	// UsageIdle means the server's statistics prove no scan of the table
	// inside the window and no session held a lock on it when read.
	UsageIdle UsageOutcome = "idle"
	// UsageRecent means the server's statistics show a scan inside the
	// window, or a session held a lock on the table when read.
	UsageRecent UsageOutcome = "recent"
	// UsageUnknown means the evidence can prove neither: the answer is not
	// idle.
	UsageUnknown UsageOutcome = "unknown"
)

// UsageReason is the typed cause behind a UsageOutcome.
type UsageReason string

const (
	// UsageReasonLockHeld means another session held a lock on the table.
	UsageReasonLockHeld UsageReason = "lock-held"
	// UsageReasonSeqScanInWindow means the last sequential scan is inside the
	// window.
	UsageReasonSeqScanInWindow UsageReason = "seq-scan-in-window"
	// UsageReasonIndexScanInWindow means the last index scan is inside the
	// window.
	UsageReasonIndexScanInWindow UsageReason = "index-scan-in-window"
	// UsageReasonScanTimesNotRecorded means the server (PostgreSQL 14 or 15)
	// does not record when a table was last scanned.
	UsageReasonScanTimesNotRecorded UsageReason = "scan-times-not-recorded"
	// UsageReasonInRecovery means the server is a standby, so its statistics
	// describe that standby, not the primary schema changes run on.
	UsageReasonInRecovery UsageReason = "in-recovery"
	// UsageReasonCountsNotTracked means track_counts is off for the reading
	// session, so the server may be collecting no table statistics.
	UsageReasonCountsNotTracked UsageReason = "counts-not-tracked"
	// UsageReasonStatsResetInWindow means the database's statistics were reset
	// inside the window, so a scan before the reset left no trace.
	UsageReasonStatsResetInWindow UsageReason = "stats-reset-in-window"
	// UsageReasonNoPartitions means the table is partitioned and has no leaf
	// partition. The server records scans only on leaf partitions, so a
	// query against it leaves no trace, and only a lock holder can show use.
	UsageReasonNoPartitions UsageReason = "no-partitions"
	// UsageReasonForeignPartition means a leaf partition is a foreign table,
	// whose scans this server keeps no statistics for.
	UsageReasonForeignPartition UsageReason = "foreign-partition"
	// UsageReasonStatsStartUnknown means the table — or, for a partitioned
	// table, one of its leaf partitions — has no recorded scan and the server
	// reports no reset time, so when counting began, and so whether "never
	// scanned" covers the window, is unknown.
	UsageReasonStatsStartUnknown UsageReason = "stats-start-unknown"
	// UsageReasonNoScanInWindow means the statistics, counted continuously since
	// before the window began, record no scan inside it.
	UsageReasonNoScanInWindow UsageReason = "no-scan-in-window"
)

// UsageVerdict is TableUsage judged against one window. It carries the
// evidence it was judged on, so a message can name what decided it.
type UsageVerdict struct {
	outcome UsageOutcome
	reason  UsageReason
	window  time.Duration
	usage   TableUsage
	// leaf indexes usage.leaves at the leaf the reason is about, or is
	// noLeaf.
	leaf int
}

// Outcome returns the verdict: idle, recent, or unknown.
func (v UsageVerdict) Outcome() UsageOutcome { return v.outcome }

// Reason returns the typed cause of the outcome.
func (v UsageVerdict) Reason() UsageReason { return v.reason }

// Window returns the window the usage was judged against.
func (v UsageVerdict) Window() time.Duration { return v.window }

// WindowStart returns the instant the window opened: the read's server
// time minus the window.
func (v UsageVerdict) WindowStart() time.Time { return v.usage.readAt.Add(-v.window) }

// Usage returns the evidence the verdict was judged on.
func (v UsageVerdict) Usage() TableUsage { return v.usage }

// Partition returns the leaf partition of a partitioned table that the
// reason is about: the one scanned most recently for a scan inside the
// window, the foreign table for foreign-partition, and the first partition
// with no recorded scan for stats-start-unknown. ok is false for every
// other reason and for an ordinary table. A lock holder names its own
// relation (LockHolder.RelationName).
func (v UsageVerdict) Partition() (p PartitionUsage, ok bool) {
	if !v.usage.partitioned || v.leaf == noLeaf {
		return PartitionUsage{}, false
	}
	return v.usage.leaves[v.leaf], true
}

// Detail renders the outcome, its reason, and the evidence that decided
// it, for messages. For a partitioned table it names the leaf partition
// each scan time comes from and the relation that decided the verdict. It
// is prose: callers branch on Outcome and Reason.
func (v UsageVerdict) Detail() string {
	u := v.usage
	detail := fmt.Sprintf("%s is %s on this server over the last %s (%s): read at %s",
		qualifiedName(u.schema, u.table), v.outcome, v.window, v.reason, u.readAt.Format(time.RFC3339))
	if u.partitioned {
		detail += fmt.Sprintf(", across %d leaf partition(s)", len(u.leaves))
	}
	detail += fmt.Sprintf(", last sequential scan %s, last index scan %s, statistics reset %s, %d lock(s) held by other sessions",
		u.formatLatestScan(PartitionUsage.seqScanTime), u.formatLatestScan(PartitionUsage.idxScanTime),
		formatOptionalTime(u.statsReset), len(u.lockHolders))
	if decidedBy := v.decidingRelation(); decidedBy != "" {
		detail += "; decided by " + decidedBy
	}
	return detail
}

// formatLatestScan renders the latest scan time across the leaves, naming
// the partition it was recorded on when the table is partitioned.
func (u TableUsage) formatLatestScan(scanTime func(PartitionUsage) *time.Time) string {
	i := latestLeaf(u.leaves, scanTime)
	if i == noLeaf {
		return formatOptionalTime(nil)
	}
	at := formatOptionalTime(scanTime(u.leaves[i]))
	if !u.partitioned {
		return at
	}
	return at + " on partition " + u.leaves[i].qualifiedName()
}

// decidingRelation names, for a partitioned table, the relation whose
// evidence decided the verdict, or is empty.
func (v UsageVerdict) decidingRelation() string {
	if !v.usage.partitioned {
		return ""
	}
	if v.reason == UsageReasonLockHeld {
		h := v.usage.lockHolders[0]
		return "a lock on " + qualifiedName(h.RelationSchema, h.RelationName)
	}
	if p, ok := v.Partition(); ok {
		return "partition " + p.qualifiedName()
	}
	return ""
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return "none recorded"
	}
	return t.Format(time.RFC3339)
}

// Judge decides whether the table was used inside the window before the
// read, on the server it was read from. It fails closed: only evidence that
// proves the table idle answers UsageIdle; anything it cannot prove is
// UsageUnknown, and evidence of use is UsageRecent. A window shorter than
// MinUsageWindow is a *UsageWindowError.
//
// Judge never decides on the write counters: they carry no time, so one
// read cannot tell a write a second ago from one a year ago. An UPDATE or
// DELETE finds its rows through a scan, which the scan times record; an
// INSERT scans nothing and is seen only while its transaction holds its
// lock. A transaction still open holds its lock on the table until it
// ends, so a scan whose time is not yet recorded is seen as a lock holder;
// one that ended within the publishing delay and holds nothing is not seen
// at all.
//
// A partitioned table is judged across all of its leaf partitions: a lock
// on the table or any partition, or a scan of any leaf inside the window,
// is use of the table, and the most recently scanned leaf names it; a leaf
// whose own evidence could not prove it idle makes the table unknown
// unless another relation shows use.
//
// The order is: a lock holder, then a scan inside the window, are evidence
// of use whatever else holds; only then can missing scan times, recovery,
// untracked counts, a reset inside the window, a partitioned table with no
// leaf partition, a foreign-table partition, or an unknown counting start
// make the answer unknown; idle is what is left.
func (u TableUsage) Judge(window time.Duration) (UsageVerdict, error) {
	if window < MinUsageWindow {
		return UsageVerdict{}, &UsageWindowError{Window: window, Min: MinUsageWindow}
	}
	outcome, reason, leaf := u.judge(u.readAt.Add(-window))
	return UsageVerdict{outcome: outcome, reason: reason, window: window, usage: u, leaf: leaf}, nil
}

// judge is Judge's decision for a window opening at windowStart, with the
// index of the leaf the reason is about, or noLeaf. An instant at
// windowStart counts as inside the window, so every edge falls toward "not
// idle".
//
// INV: RF-11 — a table is idle only when this server's statistics prove
// it for every leaf; every branch before the last returns recent or
// unknown.
func (u TableUsage) judge(windowStart time.Time) (UsageOutcome, UsageReason, int) {
	if len(u.lockHolders) > 0 {
		return UsageRecent, UsageReasonLockHeld, noLeaf
	}
	if !u.ScanTimesRecorded() {
		return UsageUnknown, UsageReasonScanTimesNotRecorded, noLeaf
	}
	if insideWindow(latestScan(u.leaves, PartitionUsage.seqScanTime), windowStart) {
		return UsageRecent, UsageReasonSeqScanInWindow, latestLeaf(u.leaves, PartitionUsage.seqScanTime)
	}
	if insideWindow(latestScan(u.leaves, PartitionUsage.idxScanTime), windowStart) {
		return UsageRecent, UsageReasonIndexScanInWindow, latestLeaf(u.leaves, PartitionUsage.idxScanTime)
	}
	if u.inRecovery {
		return UsageUnknown, UsageReasonInRecovery, noLeaf
	}
	if !u.trackCounts {
		return UsageUnknown, UsageReasonCountsNotTracked, noLeaf
	}
	// A reset clears the scan times of what it resets, and a single-table
	// reset leaves the table's index scan times standing while it moves
	// the database's reset time, so any reset inside the window makes the
	// absence of a scan unprovable.
	if insideWindow(u.statsReset, windowStart) {
		return UsageUnknown, UsageReasonStatsResetInWindow, noLeaf
	}
	// Only a partitioned table can have no leaf: an ordinary table is its
	// own. With none, nothing records a scan.
	if len(u.leaves) == 0 {
		return UsageUnknown, UsageReasonNoPartitions, noLeaf
	}
	if i := firstForeignLeaf(u.leaves); i != noLeaf {
		return UsageUnknown, UsageReasonForeignPartition, i
	}
	if i := u.leafNeverScannedSinceUnknownStart(); i != noLeaf {
		return UsageUnknown, UsageReasonStatsStartUnknown, i
	}
	return UsageIdle, UsageReasonNoScanInWindow, noLeaf
}

// insideWindow reports whether a recorded instant is at or after the
// window's start. An instant the server did not record is not inside it.
func insideWindow(t *time.Time, windowStart time.Time) bool {
	return t != nil && !t.Before(windowStart)
}

// leafNeverScannedSinceUnknownStart returns the index of the first leaf
// with no recorded scan of either kind while the server reports no reset
// time, or noLeaf. A recorded scan time is itself a lower bound on when
// counting began for that relation; with none, only the reset time could
// say, and crash recovery discards the statistics and leaves the reset time
// empty. Each leaf needs its own bound: another partition's scan time says
// nothing about when this one's counting began.
func (u TableUsage) leafNeverScannedSinceUnknownStart() int {
	if u.statsReset != nil {
		return noLeaf
	}
	return firstLeafNeverScanned(u.leaves)
}
