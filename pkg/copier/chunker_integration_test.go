package copier

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// chunkerFixture is a throwaway schema on a superuser pool. The superuser
// is a SET-usable member of every role, so the copy-and-swap proof the
// chunker demands is minted without provisioning.
type chunkerFixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newChunkerFixture(t *testing.T) chunkerFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return chunkerFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs SQL with %s standing for the fixture schema.
func (f chunkerFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema))
	require.NoError(t, err)
}

// prove mints the copy-and-swap proof for table.
func (f chunkerFixture) prove(t *testing.T, table string) preflight.CopySwapTarget {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, table, preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, table, role)
	require.NoError(t, err)
	return target
}

// sparseKeys is the key set every chunking test below cuts. Its gaps make
// key-width and row-count chunking give different answers, and the
// negative key proves the first chunk really is open below.
const sparseKeys = "(-5), (1), (2), (3), (10), (11), (12), (13), (20), (100), (101)"

func (f chunkerFixture) createSparse(t *testing.T, keyType string) preflight.CopySwapTarget {
	t.Helper()
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %%s.sparse (
			id %s PRIMARY KEY,
			note text
		)`, keyType))
	f.exec(t, "INSERT INTO %s.sparse (id) VALUES "+sparseKeys)
	return f.prove(t, "sparse")
}

// drain cuts chunks until the chunker reports the key space covered,
// checking after every cut that the frontier Cut reports is the upper bound
// of the chunk just returned.
func drain(t *testing.T, c *Chunker, db dbconn.RowQuerier) []Chunk {
	t.Helper()
	var chunks []Chunk
	for {
		chunk, ok, err := c.Next(t.Context(), db)
		require.NoError(t, err)
		if !ok {
			return chunks
		}
		cut, cutOK := c.Cut()
		require.True(t, cutOK, "a returned chunk is a cut")
		assert.Equal(t, chunk.Upper(), cut, "the frontier is the last returned chunk's upper bound")
		chunks = append(chunks, chunk)
		require.Less(t, len(chunks), 100, "chunking must terminate")
	}
}

// countIn is the behavioral oracle for a chunk: the rows the copy step will
// read for it, using the same bigint-parameter discipline as the chunker.
func (f chunkerFixture) countIn(t *testing.T, target preflight.CopySwapTarget, chunk Chunk) int64 {
	t.Helper()
	key := pgx.Identifier{target.PKColumn()}.Sanitize()
	var n int64
	err := f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM "+pgx.Identifier{target.Schema(), target.Table()}.Sanitize()+
			" WHERE "+key+" BETWEEN $1::bigint AND $2::bigint",
		chunk.Lower(), chunk.Upper()).Scan(&n)
	require.NoError(t, err)
	return n
}

func assertChunk(t *testing.T, chunk Chunk, lower, upper int64) {
	t.Helper()
	assert.Equal(t, lower, chunk.Lower(), "chunk lower bound")
	assert.Equal(t, upper, chunk.Upper(), "chunk upper bound")
}

// assertCoverage checks that consecutive chunks tile the whole int64 key
// space with no gap and no overlap, and that together they hold every row.
func (f chunkerFixture) assertCoverage(t *testing.T, target preflight.CopySwapTarget, chunks []Chunk, totalRows int64) {
	t.Helper()
	require.NotEmpty(t, chunks)
	assert.Equal(t, int64(math.MinInt64), chunks[0].Lower(), "the first chunk is open below")
	assert.Equal(t, int64(math.MaxInt64), chunks[len(chunks)-1].Upper(), "the last chunk is open above")
	var covered int64
	for i, chunk := range chunks {
		if i > 0 {
			assert.Equal(t, chunks[i-1].Upper()+1, chunk.Lower(), "chunk %d starts right after chunk %d", i, i-1)
		}
		covered += f.countIn(t, target, chunk)
	}
	assert.Equal(t, totalRows, covered, "every row belongs to exactly one chunk")
}

func TestChunkerCutsByRowCountNotKeyWidth(t *testing.T) {
	f := newChunkerFixture(t)
	target := f.createSparse(t, "bigint")

	c, err := NewChunker(target, Watermark{}, ChunkerOptions{InitialRows: 4, MinRows: 4, MaxRows: 4})
	require.NoError(t, err)
	_, cutOK := c.Cut()
	assert.False(t, cutOK, "nothing is cut before the first chunk")

	chunks := drain(t, c, f.pool)
	require.Len(t, chunks, 3)
	// Four keys each: {-5,1,2,3} then {10,11,12,13}; the final three keys
	// {20,100,101} are fewer than a chunk, so the last chunk is open above.
	assertChunk(t, chunks[0], math.MinInt64, 3)
	assertChunk(t, chunks[1], 4, 13)
	assertChunk(t, chunks[2], 14, math.MaxInt64)
	assert.Equal(t, int64(4), f.countIn(t, target, chunks[0]))
	assert.Equal(t, int64(4), f.countIn(t, target, chunks[1]))
	assert.Equal(t, int64(3), f.countIn(t, target, chunks[2]))
	f.assertCoverage(t, target, chunks, 11)
	// Every chunk records the size it was cut to, including the final one
	// that holds fewer keys than that.
	for i, chunk := range chunks {
		assert.Equal(t, int64(4), chunk.Rows(), "chunk %d cut size", i)
	}

	// Once covered, the chunker stays exhausted.
	_, ok, err := c.Next(t.Context(), f.pool)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestChunkerRowCountDividingTableStillClosesAbove(t *testing.T) {
	f := newChunkerFixture(t)
	target := f.createSparse(t, "bigint")

	// Eleven rows cut in elevens: the first chunk ends exactly on the last
	// key, and a further, empty chunk is still needed so that keys inserted
	// above it while the copy runs belong to some chunk.
	c, err := NewChunker(target, Watermark{}, ChunkerOptions{InitialRows: 11, MinRows: 11, MaxRows: 11})
	require.NoError(t, err)

	chunks := drain(t, c, f.pool)
	require.Len(t, chunks, 2)
	assertChunk(t, chunks[0], math.MinInt64, 101)
	assertChunk(t, chunks[1], 102, math.MaxInt64)
	assert.Equal(t, int64(0), f.countIn(t, target, chunks[1]))
	f.assertCoverage(t, target, chunks, 11)
}

func TestChunkerEmptyTableIsOneOpenChunk(t *testing.T) {
	f := newChunkerFixture(t)
	f.exec(t, `
		CREATE TABLE %s.empty (
			id bigint PRIMARY KEY
		)`)
	target := f.prove(t, "empty")

	c, err := NewChunker(target, Watermark{}, ChunkerOptions{})
	require.NoError(t, err)

	chunks := drain(t, c, f.pool)
	require.Len(t, chunks, 1)
	assertChunk(t, chunks[0], math.MinInt64, math.MaxInt64)
}

func TestChunkerResumesAfterWatermark(t *testing.T) {
	f := newChunkerFixture(t)
	target := f.createSparse(t, "bigint")

	// A watermark of 3 means the chunk ending at 3 was copied; the resumed
	// chunker starts at 4 and never revisits the copied keys.
	c, err := NewChunker(target, NewWatermark(3), ChunkerOptions{InitialRows: 4, MinRows: 4, MaxRows: 4})
	require.NoError(t, err)

	chunks := drain(t, c, f.pool)
	require.Len(t, chunks, 2)
	assertChunk(t, chunks[0], 4, 13)
	assertChunk(t, chunks[1], 14, math.MaxInt64)

	// A watermark at the top of the key space means the copy is complete.
	finished, err := NewChunker(target, NewWatermark(math.MaxInt64), ChunkerOptions{})
	require.NoError(t, err)
	assert.Empty(t, drain(t, finished, f.pool))
}

func TestChunkerFeedbackResizesTheNextChunk(t *testing.T) {
	f := newChunkerFixture(t)
	target := f.createSparse(t, "bigint")

	c, err := NewChunker(target, Watermark{}, ChunkerOptions{InitialRows: 2, MinRows: 2, MaxRows: 8})
	require.NoError(t, err)

	first, ok, err := c.Next(t.Context(), f.pool)
	require.NoError(t, err)
	require.True(t, ok)
	assertChunk(t, first, math.MinInt64, 1)

	// The first chunk copied in a quarter of the target time: the next one
	// doubles (the per-step cap), so it holds four keys {2,3,10,11}.
	require.NoError(t, c.Feedback(first, DefaultTargetChunkTime/4))
	assert.Equal(t, int64(4), c.Rows())
	second, ok, err := c.Next(t.Context(), f.pool)
	require.NoError(t, err)
	require.True(t, ok)
	assertChunk(t, second, 2, 11)
	assert.Equal(t, int64(4), second.Rows())

	// The second chunk took three times the target: the next one shrinks by
	// half to two keys {12,13}.
	require.NoError(t, c.Feedback(second, 3*DefaultTargetChunkTime))
	assert.Equal(t, int64(2), c.Rows())
	third, ok, err := c.Next(t.Context(), f.pool)
	require.NoError(t, err)
	require.True(t, ok)
	assertChunk(t, third, 12, 13)

	// A late report for the first chunk (two rows, a quarter of the target)
	// proposes four rows from that chunk's size, not from the current two:
	// feedback is about the chunk measured, whatever arrived since.
	require.NoError(t, c.Feedback(first, DefaultTargetChunkTime/4))
	assert.Equal(t, int64(4), c.Rows())
}

// TestChunkerSmallKeyTypes proves the bigint parameter discipline: bounds
// far outside a smallint or integer key's range are sent without error and
// the primary-key index still serves the boundary query.
func TestChunkerSmallKeyTypes(t *testing.T) {
	for _, keyType := range []string{"smallint", "integer"} {
		t.Run(keyType, func(t *testing.T) {
			f := newChunkerFixture(t)
			target := f.createSparse(t, keyType)

			c, err := NewChunker(target, Watermark{}, ChunkerOptions{InitialRows: 4, MinRows: 4, MaxRows: 4})
			require.NoError(t, err)
			chunks := drain(t, c, f.pool)
			require.Len(t, chunks, 3)
			assertChunk(t, chunks[0], math.MinInt64, 3)
			assertChunk(t, chunks[1], 4, 13)
			assertChunk(t, chunks[2], 14, math.MaxInt64)
			f.assertCoverage(t, target, chunks, 11)

			f.assertBoundaryUsesPrimaryKeyIndex(t, target)
		})
	}
}

// assertBoundaryUsesPrimaryKeyIndex plans the boundary query with sequential
// scans disabled: a plan that still walks the heap would mean the bigint
// comparison cannot use the key's index, and chunking would read the whole
// table per chunk.
func (f chunkerFixture) assertBoundaryUsesPrimaryKeyIndex(t *testing.T, target preflight.CopySwapTarget) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) }()

	_, err = tx.Exec(t.Context(), "SET LOCAL enable_seqscan = off")
	require.NoError(t, err)
	rows, err := tx.Query(t.Context(), "EXPLAIN (FORMAT TEXT) "+boundarySQL(target), math.MinInt64, 3)
	require.NoError(t, err)
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	plan := strings.Join(lines, "\n")
	assert.Contains(t, plan, "Index", "boundary query plan:\n%s", plan)
	assert.NotContains(t, plan, "Seq Scan", "boundary query plan:\n%s", plan)
}

// TestBoundarySQLFrozen pins the boundary query text on a minted proof. The
// behavioral tests above prove what the query does; this one makes any
// change to it a deliberate, reviewed edit.
func TestBoundarySQLFrozen(t *testing.T) {
	f := newChunkerFixture(t)
	target := f.createSparse(t, "bigint")

	want := fmt.Sprintf(`SELECT "id" FROM "%s"."sparse" WHERE "id" >= $1::bigint ORDER BY "id" OFFSET $2::bigint LIMIT 1`, f.schema)
	assert.Equal(t, want, boundarySQL(target))
}

func TestChunkerNextReportsQueryFailure(t *testing.T) {
	f := newChunkerFixture(t)
	target := f.createSparse(t, "bigint")
	c, err := NewChunker(target, Watermark{}, ChunkerOptions{})
	require.NoError(t, err)

	// The proof outlives the table it described: the boundary query fails
	// and the failure is returned, not swallowed as an exhausted key space.
	f.exec(t, "DROP TABLE %s.sparse")
	_, ok, err := c.Next(t.Context(), f.pool)
	require.Error(t, err)
	assert.False(t, ok)
	assert.ErrorContains(t, err, "cut chunk boundary on "+f.schema+".sparse")
}
