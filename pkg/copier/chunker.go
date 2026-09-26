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
	// ErrInvariantViolation is the sentinel a forged or empty proof wraps.
	ErrInvariantViolation = errors.New("invariant violation")
)

// ChunkerOptions bounds chunk sizing. Zero values take the defaults above.
type ChunkerOptions struct {
	// TargetChunkTime is the copy duration each chunk is sized toward (D12).
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
	if o.InitialRows == 0 {
		o.InitialRows = DefaultInitialChunkRows
	}
	if o.MinRows == 0 {
		o.MinRows = DefaultMinChunkRows
	}
	if o.MaxRows == 0 {
		o.MaxRows = DefaultMaxChunkRows
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
// covered by some chunk. That coverage is what lets the applier discard a
// captured change whose key lies above the copier's watermark: the copier
// will read the live row when it reaches that key (CO-4).
//
// Boundaries are cut by keyset: the upper bound of the next chunk is the
// key that makes the chunk hold exactly the current row count, read from
// the live table when Next is called. Sparse or dense key spaces therefore
// yield chunks of the same row count rather than the same key width.
//
// A Chunker is safe for concurrent use: Next serializes callers so chunks
// stay consecutive, and Feedback may arrive from any worker.
type Chunker struct {
	target preflight.CopySwapTarget
	opts   ChunkerOptions

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
		return nil, fmt.Errorf("%w: copy-and-swap target proof is empty", ErrInvariantViolation)
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
	if !from.Valid() {
		return math.MinInt64, false
	}
	if from.Value() == math.MaxInt64 {
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

// Next cuts the next chunk from the live table with one bounded query on
// db. ok is false once the key space is covered; the last chunk returned
// before that has the largest int64 as its upper bound.
func (c *Chunker) Next(ctx context.Context, db dbconn.RowQuerier) (chunk Chunk, ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return Chunk{}, false, nil
	}
	upper, err := c.boundary(ctx, db, c.next, c.rows)
	if err != nil {
		return Chunk{}, false, err
	}
	chunk, err = NewChunk(c.next, upper)
	if err != nil {
		return Chunk{}, false, fmt.Errorf("%w: chunk boundary: %w", ErrInvariantViolation, err)
	}
	if upper == math.MaxInt64 {
		c.done = true
	} else {
		c.next = upper + 1
	}
	return chunk, true, nil
}

// boundary returns the key that closes a chunk of rows starting at lower:
// the rows-th key at or after lower. When fewer keys remain the chunk is
// the final one and is open above.
func (c *Chunker) boundary(ctx context.Context, db dbconn.RowQuerier, lower, rows int64) (int64, error) {
	var upper *int64
	err := db.QueryRow(ctx, boundarySQL(c.target), lower, rows-1).Scan(&upper)
	if errors.Is(err, pgx.ErrNoRows) {
		return math.MaxInt64, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cut chunk boundary on %s.%s from %d: %w", c.target.Schema(), c.target.Table(), lower, err)
	}
	if upper == nil {
		// The key column is a primary key and can never be NULL; a NULL here is
		// a table that is not the one the proof described.
		return 0, fmt.Errorf("%w: chunk boundary on %s.%s from %d is NULL", ErrInvariantViolation, c.target.Schema(), c.target.Table(), lower)
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

// Feedback reports how long the last chunk took to copy so the next one is
// sized toward the target time (D12). One step changes the size by at most
// a factor of two in either direction, within the configured floor and
// ceiling.
func (c *Chunker) Feedback(elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = nextRows(c.rows, elapsed, c.opts)
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
	scaled := int64(math.Round(float64(rows) * ratio))
	return min(opts.MaxRows, max(opts.MinRows, scaled))
}
