package schemachange

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/progress"
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
// The compare reads under the verification pass's search_path, so the
// swap's own is set back as soon as the compare returns, before the keys
// it found are judged, so the path does not depend on which way the
// judgement goes. Only a compare that fails leaves it unset, and that
// error rolls the swap back. A proof that carries no key costs the swap
// no statement and reports the zero CarriedCompare.
func compareCarried(ctx context.Context, tx pgx.Tx, ready CutoverReady, clock progress.Clock) (CarriedCompare, error) {
	built := ready.built
	carried := len(ready.verified.Carried())
	if carried == 0 {
		return CarriedCompare{}, nil
	}
	started := clock.Now()
	differing, err := checksum.CompareCarried(ctx, tx, built.target, built, ready.verified)
	if err != nil {
		return CarriedCompare{}, fmt.Errorf("compare the %d keys the verification pass left for the swap of %s.%s: %w", carried, built.Schema(), built.SourceTable(), err)
	}
	compared := CarriedCompare{Keys: carried, Elapsed: clock.Now().Sub(started)}
	if err := setSearchPath(ctx, tx, built.Schema()); err != nil {
		return CarriedCompare{}, err
	}
	if len(differing) > 0 {
		// INV: CO-1
		return CarriedCompare{}, refuse(CauseCarriedDiverged, nil, "%d of the %d keys the verification pass left for the swap differ between %s.%s and its shadow %s under the swap lock, after the drain, compared in %s; the first is %d",
			len(differing), carried, built.Schema(), built.SourceTable(), built.ShadowTable(), compared.Elapsed, differing[0])
	}
	return compared, nil
}

// CarriedCompare is what the swap's compare of the keys a live pass left
// for it cost: how many keys it read on each side and how long the reads
// held the swap's ACCESS EXCLUSIVE lock for. A committed swap's compare
// found every key equal, so there is no differing count to carry; a
// compare that found one refuses the swap as cutover-carried-diverged
// instead. The zero value is a swap that compared nothing: a quiesced
// proof, a live pass that left no key, or a SwappedTable InspectSwapped
// re-derived, which cannot know what the swap compared.
type CarriedCompare struct {
	// Keys is how many carried keys the swap compared.
	Keys int
	// Elapsed is how long the compare ran, under the swap's lock, as the
	// cutover's clock measured it.
	Elapsed time.Duration
}
