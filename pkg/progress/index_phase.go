package progress

import "strings"

// indexCounter names the pg_stat_progress_create_index counter pair that
// measures one phase of an index build.
type indexCounter int

const (
	// indexCountsNothing marks a phase whose work the server does not count
	// (initializing, a sort) or whose length is set by other sessions (a
	// wait for lockers or old snapshots), so no fraction of it is known.
	indexCountsNothing indexCounter = iota
	// indexCountsBlocks marks a phase that scans a relation block by block.
	indexCountsBlocks
	// indexCountsTuples marks a phase that writes index tuples.
	indexCountsTuples
	// indexCountsBlocksOrTuples marks the build phase of an access method
	// that reports no sub-phase: blocks when the server has published a
	// block total, tuples otherwise.
	indexCountsBlocksOrTuples
)

// indexPhase is one phase of a concurrent index build: the counter pair
// that fills it and the band [start, end] of the whole build it owns, in
// percent.
type indexPhase struct {
	counter    indexCounter
	start, end float64
}

// buildingIndex is the phase the server reports while the access method
// builds the index. An access method that reports sub-phases appends one
// after a colon.
const buildingIndex = "building index"

// lookupIndexPhase maps a phase PostgreSQL reports in
// pg_stat_progress_create_index.phase for CREATE INDEX CONCURRENTLY or
// REINDEX CONCURRENTLY to its band; the cases are in execution order. The
// phase names and the counters each fills are the server's documented ones
// (https://www.postgresql.org/docs/current/progress-reporting.html#CREATE-INDEX-PROGRESS-REPORTING),
// and the "building index: …" cases are the sub-phases nbtree reports. An
// access method's sub-phase not listed here reads as the bare build phase;
// any other name is unknown.
//
// The band widths are a heuristic for wall time: the two table scans and
// the index scan dominate a build, so they own the wide bands, and a phase
// that counts nothing owns a zero-width band, because the server keeps a
// finished phase's counters in place through the phase after it and those
// must not move the percent. The last band ends below 100, so a running
// build never reads as finished.
func lookupIndexPhase(serverPhase string) (indexPhase, bool) {
	switch serverPhase {
	case "initializing", "waiting for writers before build", "building index: initializing":
		return indexPhase{indexCountsNothing, 0, 0}, true
	case "building index: scanning table":
		return indexPhase{indexCountsBlocks, 0, 35}, true
	case "building index: sorting live tuples", "building index: sorting dead tuples":
		return indexPhase{indexCountsNothing, 35, 35}, true
	case "building index: loading tuples in tree":
		return indexPhase{indexCountsTuples, 35, 50}, true
	case "waiting for writers before validation":
		return indexPhase{indexCountsNothing, 50, 50}, true
	case "index validation: scanning index":
		return indexPhase{indexCountsBlocks, 50, 65}, true
	case "index validation: sorting tuples":
		return indexPhase{indexCountsNothing, 65, 65}, true
	case "index validation: scanning table":
		return indexPhase{indexCountsBlocks, 65, 95}, true
	case "waiting for old snapshots", "waiting for readers before marking dead", "waiting for readers before dropping":
		return indexPhase{indexCountsNothing, 95, 95}, true
	}
	if serverPhase == buildingIndex || strings.HasPrefix(serverPhase, buildingIndex+": ") {
		return indexPhase{indexCountsBlocksOrTuples, 0, 50}, true
	}
	return indexPhase{}, false
}

// indexBuildFraction is the whole build's completed fraction, in [0, 0.95]:
// the reported phase's band start plus the in-band share its counter pair
// shows. ok is false for a phase lookupIndexPhase does not know.
func indexBuildFraction(serverPhase string, work Work) (fraction float64, ok bool) {
	phase, ok := lookupIndexPhase(serverPhase)
	if !ok {
		return 0, false
	}
	inBand := 0.0
	switch phase.counter {
	case indexCountsBlocks:
		inBand = ratio(work.BlocksDone, work.BlocksTotal)
	case indexCountsTuples:
		inBand = ratio(work.TuplesDone, work.TuplesTotal)
	case indexCountsBlocksOrTuples:
		if work.BlocksTotal > 0 {
			inBand = ratio(work.BlocksDone, work.BlocksTotal)
		} else {
			inBand = ratio(work.TuplesDone, work.TuplesTotal)
		}
	case indexCountsNothing:
	}
	inBand = min(inBand, 1)
	return (phase.start + inBand*(phase.end-phase.start)) / 100, true
}

// ratio is done over total, or zero while no total is published.
func ratio(done, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(done) / float64(total)
}
