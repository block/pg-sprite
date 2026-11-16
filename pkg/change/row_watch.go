package change

import (
	"context"
	"errors"
)

// ErrRowRewritten means the watched row was changed again before the
// verification could read the target, so the image handed over is no longer
// what the target holds. The caller retries.
var ErrRowRewritten = errors.New("change: watched row was rewritten during verification")

// ErrFlushIncomplete means the flush made while the reader was held did not
// empty the buffer, so the watched change may not have reached the target.
// The caller retries and must not call the difference a divergence.
var ErrFlushIncomplete = errors.New("change: parked flush did not drain the buffer")

// RowWatch names the rows a verification waits for. Match is called with a
// changed row's primary-key values in the table's key order; true fires the
// watch.
type RowWatch struct {
	Schema string
	Table  string
	Match  func(key []any) bool
}

// RowVerifier is handed the change the reader is held at: the row's key, its
// image (nil for a delete), and whether it was a delete. It runs with the
// change flushed and nothing after it delivered. An error fails the
// verification.
type RowVerifier func(ctx context.Context, key []any, image []any, deleted bool) error
