// Package checksum compares the shadow table with its source and defines
// the proofs the cutover will demand of that comparison (CO-1, CO-2, CO-3).
// A Verifier reads every key at or below the copier's landed watermark in
// chunks, digesting both tables inside one read-only REPEATABLE READ
// transaction per chunk so the two digests describe one snapshot, with
// every column cast to the type the shadow declares so a schema change that
// converts a column compares as the shadow holds it (D7). Each transaction
// runs under the table's lock session and refuses relations that are no
// longer the ones the proofs describe. A pass reports what differed; acting
// on a difference is the caller's divergence policy. VerifiedShadow and
// CleanWatermark are the proofs a clean pass will mint; their constructors
// are private to this package.
package checksum
