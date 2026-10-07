// Package applier turns decoded changes into shadow-table writes. A Buffer
// merges the stream into one Entry per primary key — the newest image or a
// delete marker, with an UPDATE overlaying only the columns it carries so an
// unchanged-TOAST marker is never mistaken for a value (CO-5, CO-8) — and
// Drain judges every key against the copier's Position: landed keys flush,
// uncut keys are discarded, in-flight keys wait for their chunk (CO-4). The
// flush itself, the column-wise upsert and the unique-move fallback, is the
// package's other half (CO-6, LK-3).
package applier
