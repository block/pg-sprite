package copier

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// Chunk sizing defaults. A chunk is sized in rows, not key width, so a
// sparse key space still produces chunks of predictable work.
const (
	// DefaultTargetChunkTime is the copy duration each chunk is sized toward.
	DefaultTargetChunkTime = 500 * time.Millisecond
	// DefaultInitialChunkRows is the first chunk's row count, before any
	// timing feedback has arrived.
	DefaultInitialChunkRows int64 = 1000
	// DefaultMinChunkRows is the floor timing feedback can shrink a chunk to.
	DefaultMinChunkRows int64 = 100
	// DefaultMaxChunkRows is the ceiling timing feedback can grow a chunk to.
	DefaultMaxChunkRows int64 = 100_000
)

// A single feedback step moves the chunk size by at most this factor in
// either direction, so one anomalous chunk cannot swing the next one wildly.
const (
	maxGrowthPerStep = 2.0
	maxShrinkPerStep = 0.5
)

var (
	// ErrInvalidChunkerOptions reports options that cannot describe a
	// bounded chunk: a non-positive target time or row count, or a floor
	// above the ceiling.
	ErrInvalidChunkerOptions = errors.New("invalid chunker options")
	// ErrInvariantViolation aliases dbconn's fail-closed error class so one
	// errors.Is check covers a breach raised here or in the connection layer.
	ErrInvariantViolation = dbconn.ErrInvariantViolation
)

// ChunkerOptions bounds chunk sizing. Zero values take the defaults above,
// fitted to whatever the caller did set: a floor, ceiling, or initial size
// given on its own pulls the other defaults around it, so setting one value
// never makes the defaults contradict it. Explicitly set values are
// validated as given.
type ChunkerOptions struct {
	// TargetChunkTime is the copy duration each chunk is sized toward: the
	// chunk-time throttle of
	// docs/copy-and-swap-design.md#d12--throttle-by-chunk-time-and-slot-lag.
	TargetChunkTime time.Duration
	// InitialRows is the first chunk's row count.
	InitialRows int64
	// MinRows is the smallest chunk feedback may produce.
	MinRows int64
	// MaxRows is the largest chunk feedback may produce.
	MaxRows int64
}

func (o ChunkerOptions) withDefaults() ChunkerOptions {
	if o.TargetChunkTime == 0 {
		o.TargetChunkTime = DefaultTargetChunkTime
	}
	// Bounds first, each fitted inside the other and around an explicit
	// initial size when only some were given, then the initial size fitted
	// inside both. Only positive values are fitted to; a non-positive one is
	// left for validate to refuse as given.
	if o.MinRows == 0 {
		o.MinRows = DefaultMinChunkRows
		if o.MaxRows > 0 {
			o.MinRows = min(o.MinRows, o.MaxRows)
		}
		if o.InitialRows > 0 {
			o.MinRows = min(o.MinRows, o.InitialRows)
		}
	}
	if o.MaxRows == 0 {
		o.MaxRows = max(DefaultMaxChunkRows, o.MinRows)
		if o.InitialRows > 0 {
			o.MaxRows = max(o.MaxRows, o.InitialRows)
		}
	}
	if o.InitialRows == 0 {
		o.InitialRows = DefaultInitialChunkRows
		if o.MinRows > 0 {
			o.InitialRows = max(o.InitialRows, o.MinRows)
		}
		if o.MaxRows > 0 {
			o.InitialRows = min(o.InitialRows, o.MaxRows)
		}
	}
	return o
}

func (o ChunkerOptions) validate() error {
	if o.TargetChunkTime <= 0 {
		return fmt.Errorf("%w: target chunk time %s must be positive", ErrInvalidChunkerOptions, o.TargetChunkTime)
	}
	for _, rows := range []struct {
		name  string
		value int64
	}{{"initial rows", o.InitialRows}, {"minimum rows", o.MinRows}, {"maximum rows", o.MaxRows}} {
		if rows.value <= 0 {
			return fmt.Errorf("%w: %s %d must be positive", ErrInvalidChunkerOptions, rows.name, rows.value)
		}
	}
	if o.MinRows > o.MaxRows {
		return fmt.Errorf("%w: minimum rows %d exceeds maximum rows %d", ErrInvalidChunkerOptions, o.MinRows, o.MaxRows)
	}
	if o.InitialRows < o.MinRows || o.InitialRows > o.MaxRows {
		return fmt.Errorf("%w: initial rows %d is outside [%d, %d]", ErrInvalidChunkerOptions, o.InitialRows, o.MinRows, o.MaxRows)
	}
	return nil
}

// Chunker cuts the proven table's primary-key space into consecutive closed
// ranges. Every key a row can carry belongs to exactly one chunk: the first
// chunk is open below (its lower bound is the smallest int64) and the last
// is open above (its upper bound is the largest), so a row that arrives
// under a key outside the table's bounds at the time chunking began is still
// covered by some chunk (CO-4 coverage).
//
// Coverage alone tells the applier which chunk a key belongs to, not whether
// the copier has read that key yet. The keys the copier will still read are
// exactly those in chunks Next has not yet returned; a chunk Next has
// returned may be in flight or landed whatever the watermark says, because
// concurrent workers land chunks out of order and the watermark advances only
// over the contiguous landed prefix. Cut reports the frontier between the two,
// so the applier discards a captured change only for a key beyond it; the
// low watermark alone is not that signal.
//
// Boundaries are cut by keyset: the upper bound of the next chunk is the
// key that makes the chunk hold exactly the current row count, read from
// the live table when Next is called. Sparse or dense key spaces therefore
// yield chunks of the same row count rather than the same key width.
//
// A Chunker is safe for concurrent use: Next serializes callers so chunks
// stay consecutive, while Cut, Rows, and Feedback never wait behind the
// boundary query a Next in progress is running.
type Chunker struct {
	target preflight.CopySwapTarget
	opts   ChunkerOptions

	// nextMu serializes Next so consecutive chunks are cut from consecutive
	// positions. It is the only lock held across the boundary query.
	nextMu sync.Mutex
	// mu guards the cursor state and is held only for reads and writes of it,
	// never across a database round trip.
	mu   sync.Mutex
	next int64 // lower bound of the next chunk; meaningful only while !done
	rows int64 // current chunk size in rows
	done bool
}

// NewChunker returns a chunker that resumes after from, or covers the whole
// key space when from is the zero watermark. It rejects a zero proof.
func NewChunker(target preflight.CopySwapTarget, from Watermark, opts ChunkerOptions) (*Chunker, error) {
	// INV: ST-6
	if target.Table() == "" {
		return nil, fmt.Errorf("%w (ST-6): copy-and-swap target proof is empty", ErrInvariantViolation)
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	c := &Chunker{target: target, opts: opts, rows: opts.InitialRows}
	c.next, c.done = startAfter(from)
	return c, nil
}

// startAfter returns the lower bound that follows the watermark. Nothing
// copied means the key space is covered from its smallest value; a
// watermark at the largest value means nothing is left to cover.
func startAfter(from Watermark) (lower int64, done bool) {
	// INV: CO-4
	if !from.Valid() {
		return math.MinInt64, false
	}
	if from.Complete() {
		return 0, true
	}
	return from.Value() + 1, false
}

// Rows returns the row count the next chunk will be cut to.
func (c *Chunker) Rows() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rows
}

// Cut reports the cut frontier: the highest key inside any chunk Next has
// returned, or that the chunker resumed past. Its zero value means no key
// has been cut. Every key above the frontier lies in a chunk the copier has
// not started reading, so a change captured for such a key can be discarded
// (CO-4); every key at or below it lies in a chunk that is in flight or
// landed and must be applied or deferred, whatever the watermark says.
func (c *Chunker) Cut() Watermark {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return NewWatermark(math.MaxInt64)
	}
	if c.next == math.MinInt64 {
		return Watermark{}
	}
	return NewWatermark(c.next - 1)
}

// Next cuts the next chunk from the live table with one bounded query on
// db. ok is false once the key space is covered; the last chunk returned
// before that has the largest int64 as its upper bound.
func (c *Chunker) Next(ctx context.Context, db dbconn.RowQuerier) (chunk Chunk, ok bool, err error) {
	c.nextMu.Lock()
	defer c.nextMu.Unlock()
	lower, rows, done := c.cursor()
	if done {
		return Chunk{}, false, nil
	}
	upper, err := c.boundary(ctx, db, lower, rows)
	if err != nil {
		return Chunk{}, false, err
	}
	chunk, err = NewChunk(lower, upper)
	if err != nil {
		return Chunk{}, false, fmt.Errorf("%w (CO-4): chunk boundary: %w", ErrInvariantViolation, err)
	}
	chunk.rows = rows
	c.advance(upper)
	return chunk, true, nil
}

// cursor snapshots the position and size the next chunk is cut with. Only
// Next moves the position, and Next is serialized, so the snapshot stays
// current for the boundary query even though mu is released while it runs.
func (c *Chunker) cursor() (lower, rows int64, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.next, c.rows, c.done
}

// advance moves the position past a chunk closed at upper, or marks the key
// space covered when that chunk was open above.
func (c *Chunker) advance(upper int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// INV: CO-4
	if upper == math.MaxInt64 {
		c.done = true
		return
	}
	c.next = upper + 1
}

// boundary returns the key that closes a chunk of rows starting at lower:
// the rows-th key at or after lower. When fewer keys remain the chunk is
// the final one and is open above.
func (c *Chunker) boundary(ctx context.Context, db dbconn.RowQuerier, lower, rows int64) (int64, error) {
	var upper *int64
	err := db.QueryRow(ctx, boundarySQL(c.target), lower, rows-1).Scan(&upper)
	if errors.Is(err, pgx.ErrNoRows) {
		// INV: CO-4
		return math.MaxInt64, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cut chunk boundary on %s.%s from %d: %w", c.target.Schema(), c.target.Table(), lower, err)
	}
	if upper == nil {
		// INV: ST-6
		// The key column is a primary key and can never be NULL; a NULL here is
		// a table that is not the one the proof described.
		return 0, fmt.Errorf("%w (ST-6): chunk boundary on %s.%s from %d is NULL", ErrInvariantViolation, c.target.Schema(), c.target.Table(), lower)
	}
	return *upper, nil
}

// boundarySQL is the keyset boundary query: with $1 the chunk's lower bound
// and $2 one less than the chunk's row count, it returns the key that makes
// the chunk hold exactly that many rows, or no row when fewer remain. The
// parameters are declared bigint whatever the key's integer type: without
// the cast PostgreSQL would infer $1 as the key's type and a bound outside a
// smallint or integer key's range could not be sent at all. The integer
// operator family compares bigint against every integer key type, so the
// primary-key index still serves the query.
func boundarySQL(target preflight.CopySwapTarget) string {
	key := pgx.Identifier{target.PKColumn()}.Sanitize()
	return "SELECT " + key +
		" FROM " + pgx.Identifier{target.Schema(), target.Table()}.Sanitize() +
		" WHERE " + key + " >= $1::bigint" +
		" ORDER BY " + key +
		" OFFSET $2::bigint LIMIT 1"
}

// Feedback reports how long chunk took to copy so the next chunk is sized
// toward the target time — the chunk-time throttle of
// docs/copy-and-swap-design.md#d12--throttle-by-chunk-time-and-slot-lag.
// The new size is scaled from the row count
// chunk was cut to, not from the current size, so reports from workers
// copying concurrently each propose a size for the work they measured
// instead of compounding on one another. One step changes the size by at
// most a factor of two in either direction, within the configured floor and
// ceiling. A chunk no chunker cut, such as a zero Chunk or one built with
// NewChunk, is refused.
func (c *Chunker) Feedback(chunk Chunk, elapsed time.Duration) error {
	// INV: ST-6
	if chunk.rows <= 0 {
		return fmt.Errorf("%w (ST-6): feedback for chunk [%d, %d] that no chunker cut", ErrInvariantViolation, chunk.Lower(), chunk.Upper())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = nextRows(chunk.rows, elapsed, c.opts)
	return nil
}

// nextRows scales rows by target/elapsed, clamped per step and to the
// configured bounds. A non-positive elapsed is treated as instantaneous,
// which grows the chunk by the maximum step.
func nextRows(rows int64, elapsed time.Duration, opts ChunkerOptions) int64 {
	ratio := maxGrowthPerStep
	if elapsed > 0 {
		ratio = float64(opts.TargetChunkTime) / float64(elapsed)
	}
	ratio = math.Min(maxGrowthPerStep, math.Max(maxShrinkPerStep, ratio))
	// Clamp before converting: a scaled value past the ceiling can also be
	// past what int64 holds, and converting it first would wrap.
	scaled := float64(rows) * ratio
	if scaled >= float64(opts.MaxRows) {
		return opts.MaxRows
	}
	if scaled <= float64(opts.MinRows) {
		return opts.MinRows
	}
	return int64(math.Round(scaled))
}
