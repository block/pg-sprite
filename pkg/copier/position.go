package copier

// KeyState is where a primary key stands relative to the copy: the CO-4
// discard rule the applier judges every captured change by.
type KeyState int

const (
	// KeyUncut lies above the cut frontier, in a chunk no worker has started
	// reading. A change captured for it can be discarded: the copier's own
	// read will see the change.
	KeyUncut KeyState = iota
	// KeyInFlight lies in a chunk a worker is reading now, or in a chunk
	// whose transaction did not commit before the run failed. A change for
	// it must wait for the chunk to land: applied earlier, a stale copy could
	// resurrect a deleted row or the applier could read a shadow row that is
	// not there yet. A resumed run re-copies such a chunk.
	KeyInFlight
	// KeyLanded lies in a chunk whose transaction committed (or that a
	// resumed run had already copied). A change for it applies immediately.
	KeyLanded
)

// String names the state for logs and error messages.
func (s KeyState) String() string {
	switch s {
	case KeyUncut:
		return "uncut"
	case KeyInFlight:
		return "in-flight"
	case KeyLanded:
		return "landed"
	}
	return "unknown"
}

// Position is one consistent snapshot of the copier's progress: the two
// frontiers CO-4 distinguishes plus the chunks between them.
type Position struct {
	// Watermark is the contiguous prefix of landed chunks — the value that is
	// checkpointed and resumed from. Its zero value means nothing has landed.
	Watermark Watermark
	// Cut is the cut frontier: the highest key of any chunk a worker has
	// claimed, so every key above it is in a chunk the copier has not started
	// reading. Its zero value means nothing has been claimed, and then every
	// key is uncut.
	Cut Watermark
	// InFlight are the claimed, unlanded chunks, in ascending key order. It is
	// empty once Run has returned nil; after a failed Run it lists the chunks
	// whose transactions did not commit, so their keys never read as landed.
	InFlight []Chunk
	// RowsInserted counts rows the copier inserted into the shadow, excluding
	// rows the applier had already written (the insert never overwrites).
	RowsInserted int64
}

// Classify reports where key stands in this snapshot. A key above the cut
// frontier is uncut whatever the watermark says; a key at or below it is in
// flight when a claimed chunk covers it and landed otherwise.
func (p Position) Classify(key int64) KeyState {
	// INV: CO-4
	if !p.Cut.Valid() || key > p.Cut.Value() {
		return KeyUncut
	}
	for _, chunk := range p.InFlight {
		if chunk.lower <= key && key <= chunk.upper {
			return KeyInFlight
		}
	}
	return KeyLanded
}
