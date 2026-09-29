// Package copier fills the shadow table from its source under the table's
// lock session. A Chunker cuts the primary-key space into row-count chunks
// that tile the whole int64 range, so every key belongs to exactly one chunk
// (CO-4) and each copy statement stays bounded (LK-3). A Copier copies those
// chunks with several workers — one guarded, never-overwriting insert per
// chunk — first clearing the shadow above the watermark when it resumes, and
// reports a Position whose Classify is the applier's rule for a captured key:
// uncut, in flight, or landed.
package copier
