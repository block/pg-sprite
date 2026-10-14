package checkpoint

import "context"

// Inspect reads the target's checkpoint before the run knows which
// statement it is resuming: it returns whatever row the target has,
// guarded on the row format alone, so a resume can read the shadow proof
// and the phase first and only then decide whether the row is its own.
// Its other outcomes are Load's — ErrNotFound for no row, ErrTableMissing
// where Ensure never ran, an IncompatibleError for a row in another format,
// and a retried transient read error that is none of those (ST-2). A row
// Inspect returns is not thereby a row the run may write: Save still
// guards every write on the whole identity, and a row whose fingerprints
// are another statement's is still refused there.
func (s *Store) Inspect(ctx context.Context, schema, table string) (Checkpoint, error) {
	return s.load(ctx, s.pool, schema, table, row.formatIncompatibility)
}
