// Package checksum compares the shadow table with its source and mints the
// proofs the cutover demands of that comparison (CO-1, CO-2, CO-3).
// A Verifier reads every key at or below the copier's landed watermark in
// chunks, digesting both tables inside one read-only REPEATABLE READ
// transaction per chunk so the two digests describe one snapshot, with
// every column cast to the type the shadow declares so a schema change that
// converts a column compares as the shadow holds it (D7). Each transaction
// runs under the table's lock session and refuses relations that are no
// longer the ones the proofs describe. Verify reports what differed; Check
// runs the same pass under a DivergencePolicy the caller states for every
// pass — abort on a difference, or recopy each differing chunk from the
// source with the copier's own statement and read it again. Only a pass
// that found no difference and repaired nothing mints a CleanWatermark,
// and a VerifiedShadow when its watermark is complete; the proofs'
// constructors are private to this package.
package checksum
