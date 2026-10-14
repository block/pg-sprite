package checksum

import "github.com/block/pg-sprite/pkg/copier"

// Mismatch is one chunk whose source and shadow digests differ: the range,
// and what each side reported for it. It says that the two sides differed
// within one snapshot, not why; the divergence policy that acts on it is
// the caller's.
type Mismatch struct {
	// Chunk is the closed key range the two digests cover.
	Chunk copier.Chunk
	// Source is the source table's digest of the chunk.
	Source Digest
	// Shadow is the shadow table's digest of the chunk.
	Shadow Digest
}

// Report is the outcome of one verification pass over the keys at or below
// a watermark: how much it read and every chunk that differed. A pass that
// finds no mismatch is clean, and only a clean pass can back a proof; the
// report itself proves nothing and is not one.
type Report struct {
	// Through is the watermark the pass verified up to: every chunk at or
	// below it was read.
	Through copier.Watermark
	// Chunks is the number of chunks the pass compared.
	Chunks int
	// Rows is the number of source rows the pass hashed.
	Rows int64
	// Mismatches lists the chunks whose digests differed, in ascending key
	// order. It is empty for a clean pass. Under a live pass these are the
	// confirmed divergences: chunks that still differed once the stream had
	// delivered everything the sweep saw.
	Mismatches []Mismatch
	// Settled is the number of chunks a live pass found differing in its
	// sweep and then read clean, or read with the shadow at exactly the
	// state the sweep saw on the source, once the catch-up had confirmed
	// past the sweep's read. A quiesced pass settles nothing.
	Settled int
	// Unsettled is the number of chunks a live pass found differing in its
	// sweep and could settle neither way within its bounds: the catch-up
	// did not confirm past the read in time, or the chunk changed on every
	// reread. They are not in Mismatches, since nothing was confirmed about
	// them, and a report that counts any is not clean. A quiesced pass
	// leaves nothing unsettled.
	Unsettled int
}

// Clean reports whether every compared chunk matched or settled, with none
// left unsettled.
func (r Report) Clean() bool { return len(r.Mismatches) == 0 && r.Unsettled == 0 }
