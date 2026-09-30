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
	// order. It is empty for a clean pass.
	Mismatches []Mismatch
}

// Clean reports whether every compared chunk matched.
func (r Report) Clean() bool { return len(r.Mismatches) == 0 }
