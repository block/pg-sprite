package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// markPosition writes a WAL marker from inside tx and returns the position
// just past it (decode.MarkWAL): once the catch-up has confirmed past that
// position, every change tx's snapshot saw is in the shadow.
func (v *Verifier) markPosition(ctx context.Context, tx pgx.Tx) (decode.LSN, error) {
	lsn, err := decode.MarkWAL(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("read of %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	return lsn, nil
}
