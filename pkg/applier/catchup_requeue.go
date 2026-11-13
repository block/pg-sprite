package applier

// logRequeue warns, once per cycle that requeued its batch, with the
// target and the running count the cycle recorded, so a caller that does
// not poll Status sees the condition while it lasts. A batch refused by a
// constraint waits for the copy to move; one the server rolled back as a
// deadlock victim is flushed again next cycle, and the consecutive count
// against the bound says how close the catch-up is to ending. Only counts
// are logged, never the batch's rows.
func (c *Catchup) logRequeue(outcome cycleOutcome, status Status) {
	switch {
	case outcome.deferred:
		c.opts.Logger.Warn("catch-up batch refused by a shadow constraint; it will be flushed again once the copy moves",
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
