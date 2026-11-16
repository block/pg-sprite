package change

import (
	"context"
	"errors"
	"time"
)

// ErrPositionNotFound is returned by StartFromPosition when the source can no
// longer resume from the requested position. Wrapped with %w so callers can
// errors.Is against it.
var ErrPositionNotFound = errors.New("change: cannot resume from position; it is no longer available on the source")

// Source is a stream of row changes from a source database, pushed to
// subscriptions. It declares the same methods as Spirit's change.Source
// (https://github.com/block/spirit/blob/fc9cb5f4feed5b172da761865ff2809e4da9d912/pkg/change/source.go)
// over the local declarations of the types those methods take; a change to
// either side is carried to the other by hand.
//
// Lifecycle: construct → AddSubscription(...)* → Start(ctx) or
// StartFromPosition(ctx, pos) → Flush / BlockWait / FlushUnderTableLock as
// needed → Stop() → Close().
//
// Events flow push-style: a row change to a subscribed table is handed to
// the subscription's HasChanged. Positions are opaque strings whose encoding
// belongs to the implementation.
type Source interface {
	// AddSubscription builds a subscription for (currentTable, newTable,
	// chunker) and registers it. Row changes to currentTable are pushed to
	// it. Must be called before Start / StartFromPosition.
	AddSubscription(currentTable, newTable *TableInfo, chunker MappedChunker) error

	// Start begins streaming from the current source head and returns once
	// the reader is running; the stream continues until Close or until ctx
	// ends.
	Start(ctx context.Context) error

	// StartFromPosition resumes from a position Position returned. A
	// position the source can no longer resume from is returned wrapped
	// with ErrPositionNotFound.
	StartFromPosition(ctx context.Context, pos string) error

	// Position is the latest position safe to resume from, with no server
	// I/O. It advances only at commit boundaries and only once every change
	// below it has been flushed; "" before anything has been observed.
	Position() string

	// CurrentPosition reads the source server's current head, in the same
	// encoding as Position, so the result is a StartFromPosition input. It
	// needs no running feed.
	CurrentPosition(ctx context.Context) (string, error)

	// Flush flushes every subscription's buffered changes to its target and
	// blocks until that completes or ctx ends.
	Flush(ctx context.Context) error

	// FlushUnderTableLock is the cutover-time Flush: the caller holds a
	// lock per target server, and each subscription's applier executes
	// that target's statements under that target's lock.
	FlushUnderTableLock(ctx context.Context, locks []TableLock) error

	// BlockWait blocks until every change the source committed up to the
	// call has been delivered to its subscription, or ctx ends.
	BlockWait(ctx context.Context) error

	// GetDeltaLen is the number of changes pending across all
	// subscriptions.
	GetDeltaLen() int

	// FlushResidual is what the most recently completed flush left behind:
	// residual is the pending-change count right after it, and flushes a
	// monotonic count of completed flushes. Both are 0 before the first.
	FlushResidual() (residual, flushes int)

	// SetWatermarkOptimization turns the subscriptions' high/low watermark
	// optimization on or off.
	SetWatermarkOptimization(ctx context.Context, enabled bool) error

	// StartPeriodicFlush flushes at the given interval in a background
	// goroutine until StopPeriodicFlush or Close. A second call while one is
	// running, or a call after Close, does nothing.
	StartPeriodicFlush(ctx context.Context, interval time.Duration)

	// StopPeriodicFlush stops the periodic flush and waits for it. Safe when
	// none is running.
	StopPeriodicFlush()

	// AllChangesFlushed reports whether every change received has been
	// handed to its subscription and flushed.
	AllChangesFlushed() bool

	// VerifyRowAtNextChange waits for the next change to a row matching
	// watch, holds the reader at it, flushes so the target holds that image,
	// and calls verify with it. The reader is released before it returns.
	// It returns ErrRowRewritten if a further change to the row was admitted
	// before verify ran, ErrFlushIncomplete if the flush could not land the
	// change, and ctx.Err() if no change arrived in time.
	VerifyRowAtNextChange(ctx context.Context, watch RowWatch, verify RowVerifier) error

	// FeedStats reports the flush shape and timing and how far the reader
	// has read.
	FeedStats() FeedStats

	// Stop ends delivery of changes to subscriptions; the source keeps
	// reading and tracking its position, and Flush, BlockWait,
	// AllChangesFlushed, and Position keep working. One-way, idempotent, and
	// non-blocking.
	Stop()

	// Close releases every resource, stopping and joining the reader and
	// any periodic flush. Safe to call more than once.
	Close()
}
