package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/checksum"
)

// compareCarried finishes a live pass's proof inside the swap transaction:
// it reads every key the pass left for the swap on the source and on the
// shadow and refuses the swap when any of them differs. The pass could not
// judge these keys because they changed at every reread; by this point the
// transaction holds ACCESS EXCLUSIVE on both relations and the drain has
// applied every change the stream carried, so the shadow has no writer
// left to wait for and a key that differs is a row the live table would
// hold wrong. The refusal rolls the swap back with the source live.
//
// The compare reads under the verification pass's settings, so it restores
// the search_path the rest of the swap's statements run under before it
// returns. A proof that carries no key costs the swap no statement.
func compareCarried(ctx context.Context, tx pgx.Tx, ready CutoverReady) error {
	built := ready.built
	carried := len(ready.verified.Carried())
	if carried == 0 {
		return nil
	}
	differing, err := checksum.CompareCarried(ctx, tx, built.target, built, ready.verified)
	if err != nil {
		return fmt.Errorf("compare the %d keys the verification pass left for the swap of %s.%s: %w", carried, built.Schema(), built.SourceTable(), err)
	}
	if len(differing) > 0 {
		// INV: CO-1
		return refuse(CauseCarriedDiverged, nil, "%d of the %d keys the verification pass left for the swap differ between %s.%s and its shadow %s under the swap lock, after the drain; the first is %d",
			len(differing), carried, built.Schema(), built.SourceTable(), built.ShadowTable(), differing[0])
	}
	return setSearchPath(ctx, tx, built.Schema())
}
