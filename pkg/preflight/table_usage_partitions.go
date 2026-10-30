package preflight

import (
	"cmp"
	"time"
)

// PartitionUsage is the evidence read for one leaf partition of a
// partitioned table: a partition that holds rows, at any level below the
// table, the default partition included. A sub-partitioned partition holds
// no rows and has no PartitionUsage of its own; its leaves do. It is only
// built by ReadTableUsage.
type PartitionUsage struct {
	schema         string
	name           string
	oid            uint32
	foreign        bool
	lastSeqScan    *time.Time
	lastIdxScan    *time.Time
	seqScans       int64
	idxScans       int64
	tuplesInserted int64
	tuplesUpdated  int64
	tuplesDeleted  int64
}

// Schema returns the partition's schema.
func (p PartitionUsage) Schema() string { return p.schema }

// Name returns the partition's name.
func (p PartitionUsage) Name() string { return p.name }

// OID returns the partition's relation OID.
func (p PartitionUsage) OID() uint32 { return p.oid }

// Foreign reports whether the partition is a foreign table. Its rows live
// on another server, and this server keeps no scan statistics for it.
func (p PartitionUsage) Foreign() bool { return p.foreign }

// LastSeqScan returns when the server last recorded a sequential scan of
// the partition. ok is false when it recorded none since its statistics
// began, for a foreign table, and always on a server that does not record
// scan times.
func (p PartitionUsage) LastSeqScan() (at time.Time, ok bool) { return derefTime(p.lastSeqScan) }

// LastIdxScan returns when the server last recorded an index scan on any of
// the partition's indexes. ok is false when it recorded none, when the
// partition has no index, for a foreign table, and always on a server that
// does not record scan times.
func (p PartitionUsage) LastIdxScan() (at time.Time, ok bool) { return derefTime(p.lastIdxScan) }

// SeqScans returns the cumulative count of sequential scans of the
// partition (0 for a foreign table).
func (p PartitionUsage) SeqScans() int64 { return p.seqScans }

// IdxScans returns the cumulative count of index scans on the partition's
// indexes (0 with no index, and for a foreign table).
func (p PartitionUsage) IdxScans() int64 { return p.idxScans }

func (p PartitionUsage) seqScanTime() *time.Time { return p.lastSeqScan }

func (p PartitionUsage) idxScanTime() *time.Time { return p.lastIdxScan }

// qualifiedName renders the partition as schema.name for messages.
func (p PartitionUsage) qualifiedName() string { return qualifiedName(p.schema, p.name) }

func comparePartitions(a, b PartitionUsage) int {
	return cmp.Or(cmp.Compare(a.schema, b.schema), cmp.Compare(a.name, b.name))
}

// noLeaf is the leaf index that names no leaf.
const noLeaf = -1

// latestLeaf returns the index of the leaf whose scan time, as scanTime
// picks it, is the latest, or noLeaf when no leaf has one recorded. Of
// leaves with the same time the first wins.
func latestLeaf(leaves []PartitionUsage, scanTime func(PartitionUsage) *time.Time) int {
	latest := noLeaf
	for i, leaf := range leaves {
		t := scanTime(leaf)
		if t == nil {
			continue
		}
		if latest == noLeaf || t.After(*scanTime(leaves[latest])) {
			latest = i
		}
	}
	return latest
}

// latestScan returns the latest scan time across the leaves, as scanTime
// picks it, or nil when none is recorded.
func latestScan(leaves []PartitionUsage, scanTime func(PartitionUsage) *time.Time) *time.Time {
	i := latestLeaf(leaves, scanTime)
	if i == noLeaf {
		return nil
	}
	return scanTime(leaves[i])
}

func sumLeaves(leaves []PartitionUsage, count func(PartitionUsage) int64) int64 {
	var sum int64
	for _, leaf := range leaves {
		sum += count(leaf)
	}
	return sum
}

// firstForeignLeaf returns the index of the first leaf that is a foreign
// table, or noLeaf.
func firstForeignLeaf(leaves []PartitionUsage) int {
	for i, leaf := range leaves {
		if leaf.foreign {
			return i
		}
	}
	return noLeaf
}

// firstLeafNeverScanned returns the index of the first leaf with no
// recorded scan of either kind, or noLeaf.
func firstLeafNeverScanned(leaves []PartitionUsage) int {
	for i, leaf := range leaves {
		if leaf.neverScanned() {
			return i
		}
	}
	return noLeaf
}

func (p PartitionUsage) neverScanned() bool {
	return p.lastSeqScan == nil && p.lastIdxScan == nil
}
