package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// observation is one two-sided read of a chunk under a live pass: every
// row each side holds in the chunk, hashed, and the position of the marker
// written in the same transaction. Every transaction visible to the
// snapshot committed before the marker, so once the catch-up has confirmed
// past it, every source change the read saw is in the shadow. The chunk's
// digests are derived from the rows.
type observation struct {
	chunk  copier.Chunk
	source rowSet
	shadow rowSet
	lsn    decode.LSN
}

// digests returns the two chunk digests the observation's rows derive.
func (o observation) digests() (source, shadow Digest) {
	return o.source.digest(), o.shadow.digest()
}

// mismatch reports the chunk as the observation saw it.
func (o observation) mismatch() Mismatch {
	source, shadow := o.digests()
	return Mismatch{Chunk: o.chunk, Source: source, Shadow: shadow}
}

// settlement is what settling a differing chunk concluded: the last read
// of it, and how many of its keys were confirmed divergent. A settlement
// with no divergence is a chunk the stream has delivered: every key read
// equal, or read on the shadow as an earlier confirmed read saw it on the
// source.
type settlement struct {
	last     observation
	diverged int
}

// settle decides a chunk found differing, key by key: wait for the catch-up
// to confirm past the position the chunk was read at, read every row of it
// again in a fresh snapshot, and judge each key across the two reads. The
// first reread judges every key either read holds on either side; each
// reread after it judges only the keys the one before left hot, since a
// key settled once is concluded as of that read and the stream carries
// every write to it since. Any key confirmed divergent concludes the chunk
// at once. A chunk with hot keys left is read again from its latest read,
// up to live.Attempts rereads in all, counting the rereads the caller
// already spent on the chunk; a wait that runs out or the attempts
// exhausted is an *UnresolvedError.
func (v *Verifier) settle(ctx context.Context, pool *pgxpool.Pool, stmts statements, prior observation, rereads int, live LiveOptions) (settlement, error) {
	var pending []int64
	for {
		if err := v.awaitConfirmed(ctx, prior, rereads, live); err != nil {
			return settlement{}, err
		}
		now, err := v.observe(ctx, pool, stmts, prior.chunk)
		if err != nil {
			return settlement{}, err
		}
		v.countReread()
		v.countSettleRows(now)
		rereads++
		if pending == nil {
			pending = unionKeys(prior.source, prior.shadow, now.source, now.shadow)
		}
		hot, diverged := judgeKeys(prior, now, pending)
		if diverged > 0 {
			return settlement{last: now, diverged: diverged}, nil
		}
		if len(hot) == 0 {
			return settlement{last: now}, nil
		}
		if rereads >= live.Attempts {
			source, shadow := now.digests()
			return settlement{}, &UnresolvedError{
				Chunk: now.chunk, Cause: CauseHot, Rereads: rereads, Keys: len(hot), HotKeys: firstHotKeys(hot),
				Source: source, Shadow: shadow,
				Captured: now.lsn, Confirmed: live.Confirmed.Confirmed(),
			}
		}
		pending = hot
		prior = now
	}
}

// awaitConfirmed waits until the catch-up has confirmed past the position
// prior was read at, sleeping live.Poll between reads of the confirmed
// position for as many polls as fit in live.SettleWait. The bound is a
// count of sleeps, not a clock reading, so the wait ends at the same poll
// whatever sleep the caller injected.
func (v *Verifier) awaitConfirmed(ctx context.Context, prior observation, rereads int, live LiveOptions) error {
	for polled := 0; ; polled++ {
		confirmed := live.Confirmed.Confirmed()
		if confirmed >= prior.lsn {
			return nil
		}
		if polled >= live.polls() {
			source, shadow := prior.digests()
			return &UnresolvedError{
				Chunk: prior.chunk, Cause: CauseLagging, Rereads: rereads,
				Source: source, Shadow: shadow,
				Captured: prior.lsn, Confirmed: confirmed,
			}
		}
		if err := live.Sleep(ctx, live.Poll); err != nil {
			return fmt.Errorf("wait for the catch-up to confirm past %s for chunk [%d, %d] of %s.%s: %w", prior.lsn, prior.chunk.Lower(), prior.chunk.Upper(), v.target.Schema(), v.target.Table(), err)
		}
	}
}

// observe reads every row of chunk on both sides and marks the position in
// one guarded snapshot of its own. It counts nothing: the caller knows what
// the read is for.
func (v *Verifier) observe(ctx context.Context, pool *pgxpool.Pool, stmts statements, chunk copier.Chunk) (observation, error) {
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
	now, err := v.readRange(ctx, tx, stmts, chunk)
	if err != nil {
		return observation{}, err
	}
	if now.lsn, err = v.markPosition(ctx, tx); err != nil {
		return observation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return observation{}, fmt.Errorf("commit reread of chunk [%d, %d] of %s.%s: %w", chunk.Lower(), chunk.Upper(), v.target.Schema(), v.target.Table(), err)
	}
	return now, nil
}

// readRange reads every row of chunk on both sides in tx. The observation
// carries no position; the caller marks one when it needs it.
func (v *Verifier) readRange(ctx context.Context, tx pgx.Tx, stmts statements, chunk copier.Chunk) (observation, error) {
	source, err := readRows(ctx, tx, stmts.source.rows, chunk.Lower(), chunk.Upper())
	if err != nil {
		return observation{}, fmt.Errorf("read rows of chunk [%d, %d] of %s.%s: %w", chunk.Lower(), chunk.Upper(), v.shadow.Schema(), v.shadow.SourceTable(), err)
	}
	shadow, err := readRows(ctx, tx, stmts.shadow.rows, chunk.Lower(), chunk.Upper())
	if err != nil {
		return observation{}, fmt.Errorf("read rows of chunk [%d, %d] of shadow %s.%s: %w", chunk.Lower(), chunk.Upper(), v.shadow.Schema(), v.shadow.ShadowTable(), err)
	}
	return observation{chunk: chunk, source: source, shadow: shadow}, nil
}
