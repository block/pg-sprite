package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// observation is one two-sided read of a chunk under a live pass: the two
// digests and the server's insert position read in the same snapshot.
// Every transaction visible to the snapshot committed at or below that
// position, so once the catch-up has confirmed past it, every source change
// the read saw is in the shadow.
type observation struct {
	chunk  copier.Chunk
	source Digest
	shadow Digest
	lsn    decode.LSN
}

// verdict is what two reads of a differing chunk say together.
type verdict int

const (
	// verdictEqual is a reread whose two sides agree.
	verdictEqual verdict = iota
	// verdictSettled is a reread whose shadow equals what the earlier read
	// saw on the source: the shadow has reached the state the source was
	// read in, and the source has moved on since, which the stream carries.
	verdictSettled
	// verdictHot is a reread where the source or the shadow changed since
	// the earlier read and the two sides still differ: nothing can be
	// concluded, and the chunk is read again.
	verdictHot
	// verdictDiverged is a reread where neither side changed since the
	// earlier read, the catch-up confirmed past that read, and the two sides
	// still differ: the stream has delivered everything the source holds
	// and the shadow does not match it.
	verdictDiverged
)

// judge compares a chunk's reread with the read before it. The reread was
// taken after the catch-up confirmed past the earlier read's position.
func judge(prior, now observation) verdict {
	if now.shadow == now.source {
		return verdictEqual
	}
	if now.shadow == prior.source {
		return verdictSettled
	}
	if now.source != prior.source {
		return verdictHot
	}
	if now.shadow != prior.shadow {
		return verdictHot
	}
	return verdictDiverged
}

// settle decides a chunk the sweep found differing: wait for the catch-up
// to confirm past the position the chunk was read at, read it again in a
// fresh snapshot, and judge the two reads. A hot chunk is read again from
// its latest read, up to live.Attempts rereads in all; a wait that runs out
// or the attempts exhausted is an *UnresolvedError. The returned
// observation is the last read.
func (v *Verifier) settle(ctx context.Context, pool *pgxpool.Pool, sourceSQL, shadowSQL string, prior observation, live LiveOptions) (verdict, observation, error) {
	for rereads := 0; ; {
		if err := v.awaitConfirmed(ctx, prior, rereads, live); err != nil {
			return 0, prior, err
		}
		now, err := v.observe(ctx, pool, sourceSQL, shadowSQL, prior.chunk)
		if err != nil {
			return 0, prior, err
		}
		v.countReread()
		rereads++
		result := judge(prior, now)
		if result != verdictHot {
			return result, now, nil
		}
		if rereads >= live.Attempts {
			return 0, now, &UnresolvedError{
				Chunk: now.chunk, Cause: CauseHot, Rereads: rereads,
				Source: now.source, Shadow: now.shadow,
				Captured: now.lsn, Confirmed: live.Confirmed.Confirmed(),
			}
		}
		prior = now
	}
}

// awaitConfirmed waits until the catch-up has confirmed past the position
// prior was read at, polling at live.Poll for at most live.SettleWait.
func (v *Verifier) awaitConfirmed(ctx context.Context, prior observation, rereads int, live LiveOptions) error {
	deadline := v.opts.Clock.Now().Add(live.SettleWait)
	for {
		confirmed := live.Confirmed.Confirmed()
		if confirmed >= prior.lsn {
			return nil
		}
		if !v.opts.Clock.Now().Before(deadline) {
			return &UnresolvedError{
				Chunk: prior.chunk, Cause: CauseLagging, Rereads: rereads,
				Source: prior.source, Shadow: prior.shadow,
				Captured: prior.lsn, Confirmed: confirmed,
			}
		}
		if err := live.Sleep(ctx, live.Poll); err != nil {
			return fmt.Errorf("wait for the catch-up to confirm past %s for chunk [%d, %d] of %s.%s: %w", prior.lsn, prior.chunk.Lower(), prior.chunk.Upper(), v.target.Schema(), v.target.Table(), err)
		}
	}
}

// observe reads both sides of chunk and the server's insert position in one
// guarded snapshot of its own. It counts nothing: the caller knows what
// the read is for.
func (v *Verifier) observe(ctx context.Context, pool *pgxpool.Pool, sourceSQL, shadowSQL string, chunk copier.Chunk) (observation, error) {
	tx, err := v.begin(ctx, pool, snapshotRead())
	if err != nil {
		return observation{}, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	source, shadow, err := v.digestRange(ctx, tx, sourceSQL, shadowSQL, chunk.Lower(), chunk.Upper())
	if err != nil {
		return observation{}, err
	}
	lsn, err := v.insertPosition(ctx, tx)
	if err != nil {
		return observation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return observation{}, fmt.Errorf("commit reread of chunk [%d, %d] of %s.%s: %w", chunk.Lower(), chunk.Upper(), v.target.Schema(), v.target.Table(), err)
	}
	return observation{chunk: chunk, source: source, shadow: shadow, lsn: lsn}, nil
}

// insertPosition reads the server's insert position inside tx: every
// transaction the snapshot sees committed at or below it. The insert
// position, not the write position: under synchronous_commit = off a
// commit is visible to a snapshot before its WAL is written, so only the
// insert position bounds every commit the snapshot sees.
func (v *Verifier) insertPosition(ctx context.Context, tx pgx.Tx) (decode.LSN, error) {
	var text string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_current_wal_insert_lsn()::text`).Scan(&text); err != nil {
		return 0, fmt.Errorf("read the server's insert position for %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	lsn, err := decode.ParseLSN(text)
	if err != nil {
		return 0, fmt.Errorf("read the server's insert position for %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	return lsn, nil
}
