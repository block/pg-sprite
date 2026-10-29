package checksum

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
)

// SQLSTATE codes the server raises when a recopy's insert lands a row whose
// value another shadow row already holds under a unique index or an
// exclusion constraint.
const (
	sqlstateUniqueViolation    = "23505"
	sqlstateExclusionViolation = "23P01"
)

// DefaultRecopyAttempts is the number of times a recopy pass compares and
// recopies before it stops on a recopy that keeps colliding, when Options
// leaves RecopyAttempts zero.
const DefaultRecopyAttempts = 5

// collided reports whether err is the server refusing a recopy's insert
// because a shadow row already holds one of its values under a unique index
// or an exclusion constraint. The primary key cannot be the index: the
// insert's ON CONFLICT (pk) DO NOTHING skips a row already there (CO-4),
// and the recopy's delete has cleared the chunk's keys before it.
func collided(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlstateUniqueViolation || pgErr.Code == sqlstateExclusionViolation
}

// recopyUntilClear is the recopy phase of a pass under DivergenceRecopy:
// it recopies every chunk the comparison found differing, and when the
// recopy collides it compares again and recopies what that comparison
// finds, until a recopy commits or the attempts run out. A collision is a
// source row in a differing chunk whose unique value a shadow row in a
// chunk the comparison read equal still holds: the source moved the value
// between the two chunks after the comparison read the clean one and
// before the recopy read the differing one. Nothing else writes the shadow
// under this policy, so the colliding row stays until a recopy clears its
// chunk, and the row's chunk now differs from the source: the next
// comparison finds it, and the next recopy deletes both chunks before it
// puts either back. The comparison is the wait between attempts; there is
// no other. Comparing again clears a collision only when the colliding row
// lies inside the comparison: a shadow row above the watermark the pass
// reads through is never compared, so a collision with one repeats on
// every attempt and the pass ends in its error — which is why a caller
// that compares through a partial watermark clears the shadow above it
// first (copier.ClearAbove). The Outcome returned
// carries the comparison the committed recopy acted on, the repairs it
// committed, and the attempts that collided, with the error when the last
// one did; a recopy that fails for
// any other reason ends the pass with that error, as a first attempt's
// would.
func (v *Verifier) recopyUntilClear(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, report Report) (Outcome, error) {
	outcome := Outcome{Report: report}
	for attempt := 1; ; attempt++ {
		repairs, err := v.recopyAll(ctx, pool, outcome.Report.Mismatches)
		outcome.Repairs = repairs
		if err == nil {
			return outcome, nil
		}
		if !collided(err) {
			return outcome, err
		}
		outcome.Collisions++
		if attempt == v.opts.RecopyAttempts {
			return outcome, fmt.Errorf("recopy of %d chunks of %s.%s collided with a shadow row holding one of their unique values on each of %d attempts: %w", len(outcome.Report.Mismatches), v.target.Schema(), v.target.Table(), attempt, err)
		}
		compared, err := v.verify(ctx, pool, through)
		if err != nil {
			return outcome, err
		}
		if compared.Clean() {
			proved := v.prove(compared, false)
			proved.Collisions = outcome.Collisions
			return proved, nil
		}
		outcome.Report = compared
	}
}
