// Package applier turns decoded changes into shadow-table writes. A Buffer
// merges the stream into one Entry per primary key — the newest image or a
// delete marker, with an UPDATE overlaying only the columns it carries so an
// unchanged-TOAST marker is never mistaken for a value (CO-5, CO-8) — and
// Drain judges every key against the copier's Position: landed keys flush,
// uncut keys are discarded, in-flight keys wait for their chunk (CO-4). A
// Flusher applies the drained Batch in one guarded transaction under the
// table lock: it first reads a completion for every moved marker-bearing
// image the batch names, from the old key's shadow row or from the source
// when the old key was reused, and hands each back as a HeldImage with the
// WAL position of the read — the row read can be newer than the stream, so
// the buffer holds the image (Hold) until the stream has passed the read
// (Release) and flushes it then — then writes column-wise, every delete
// before any image: a delete, an upsert of a whole image, or an UPDATE of
// only the columns an image carries. On a unique or exclusion violation it
// falls back to deleting every key in the batch and inserting every
// surviving whole row (CO-6, CO-8). A batch a constraint still refuses comes
// back as ErrBatchDeferred for the caller to Requeue.
package applier
