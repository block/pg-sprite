package applier

// logRequeue logs a cycle that requeued its batch, with the target and the
// running count the cycle recorded, so a caller that does not poll Status
// sees the condition while it lasts. Only counts are logged, never the
// batch's rows.
//
// A batch refused by a constraint waits for the copy to move, which is the
// designed convergence path and can last as long as the copy takes to land
// the chunk in the way, one requeue a cycle. It logs at Info, and only when
// the run's count reaches a power of two (the 1st, 2nd, 4th, 8th refusal
// ...), so a long wait writes a few lines rather than one a cycle. A batch
// the server rolled back as a deadlock victim logs at Warn every time: the
// run ends after DeadlockAttempts of them in a row, and the consecutive
// count against that bound says how close it is.
func (c *Catchup) logRequeue(outcome cycleOutcome, status Status) {
	switch {
	case outcome.deferred:
		if !isPowerOfTwo(status.BatchesDeferred) {
			return
		}
		c.opts.Logger.Info("catch-up batch refused by a shadow constraint; it will be flushed again once the copy moves",
			"schema", c.flusher.target.Schema(),
			"table", c.flusher.target.Table(),
			"batches_deferred", status.BatchesDeferred,
			"entries", len(outcome.batch.Entries))
	case outcome.deadlocked:
		c.opts.Logger.Warn("catch-up flush rolled back as a deadlock victim; the batch will be flushed again next cycle",
			"schema", c.flusher.target.Schema(),
			"table", c.flusher.target.Table(),
			"batches_deadlocked", status.BatchesDeadlocked,
			"consecutive_deadlocks", c.deadlocks,
			"deadlock_attempts", c.opts.DeadlockAttempts,
			"entries", len(outcome.batch.Entries))
	}
}

// isPowerOfTwo reports whether n is 1, 2, 4, 8, and so on.
func isPowerOfTwo(n uint64) bool {
	return n != 0 && n&(n-1) == 0
}
