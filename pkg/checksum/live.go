package checksum

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// ConfirmedSource answers how far the catch-up has applied the stream: the
// position it last confirmed to the server, below which every change to a
// landed key is committed in the shadow. The applier's Catchup is one. An
// implementation of its own must answer from memory without blocking — a
// live pass reads it from its own goroutine once every LiveOptions.Poll
// while it waits — must never answer lower than it already has, and must
// follow the walsender's own position, not only the transactions it
// delivers: the pass waits for the position of a marker it wrote, which
// no transaction carries, and a source that advances only on applied
// transactions never reaches it on a quiet table.
type ConfirmedSource interface {
	Confirmed() decode.LSN
}

// ErrVerificationUnresolved is the sentinel an UnresolvedError unwraps to:
// a live pass could not decide whether a differing chunk is a divergence
// or a chunk the stream has not yet settled.
var ErrVerificationUnresolved = errors.New("live verification could not settle a chunk")

// UnresolvedCause says why a live pass gave up on a chunk.
type UnresolvedCause string

const (
	// CauseLagging is a catch-up that did not confirm past the position the
	// chunk's digests were read at within LiveOptions.SettleWait.
	CauseLagging UnresolvedCause = "lagging"
	// CauseHot is a chunk whose source or shadow changed on every one of
	// LiveOptions.Attempts rereads, so no two reads could be compared.
	CauseHot UnresolvedCause = "hot"
)

// UnresolvedError is a chunk a live pass could neither read clean nor
// confirm divergent. It is not a finding about the data: the shadow is as
// the pass found it, and the caller decides whether to run the pass again
// or wait for the source to quiet down. It unwraps to
// ErrVerificationUnresolved.
type UnresolvedError struct {
	// Chunk is the closed key range the pass could not settle.
	Chunk copier.Chunk
	// Cause says whether the catch-up fell behind or the chunk kept moving.
	Cause UnresolvedCause
	// Rereads is the number of fresh-snapshot rereads the pass made of the
	// chunk before it gave up.
	Rereads int
	// Source and Shadow are the two digests of the pass's last read.
	Source Digest
	Shadow Digest
	// Captured is the position of the marker the last read wrote to the
	// WAL; every change the read saw committed before it, and the pass was
	// waiting for the catch-up to confirm past it.
	Captured decode.LSN
	// Confirmed is the catch-up's confirmed position when the pass gave up.
	Confirmed decode.LSN
}

// Error says what the pass could not decide and what to do about it: a
// lagging catch-up wants the pass run again once it has caught up, and a
// hot chunk wants a quieter source.
func (e *UnresolvedError) Error() string {
	switch e.Cause {
	case CauseLagging:
		return fmt.Sprintf("chunk [%d, %d] read at %s is unsettled: the catch-up has confirmed only %s after %s; run the pass again once the catch-up has confirmed past the read",
			e.Chunk.Lower(), e.Chunk.Upper(), e.Captured, e.Confirmed, rereadsPhrase(e.Rereads))
	case CauseHot:
		return fmt.Sprintf("chunk [%d, %d] changed on every one of %s: source %d rows, shadow %d rows at %s; the source is changing faster than the pass can compare it, so run the pass again when it is quieter",
			e.Chunk.Lower(), e.Chunk.Upper(), rereadsPhrase(e.Rereads), e.Source.Rows, e.Shadow.Rows, e.Captured)
	default:
		return fmt.Sprintf("chunk [%d, %d] is unsettled after %s", e.Chunk.Lower(), e.Chunk.Upper(), rereadsPhrase(e.Rereads))
	}
}

// rereadsPhrase counts rereads in words that agree with the number.
func rereadsPhrase(n int) string {
	if n == 1 {
		return "1 reread"
	}
	return fmt.Sprintf("%d rereads", n)
}

// Unwrap makes every UnresolvedError match ErrVerificationUnresolved.
func (e *UnresolvedError) Unwrap() error { return ErrVerificationUnresolved }

// Default bounds of a live pass's settling.
const (
	// DefaultSettleAttempts is how many fresh rereads a live pass gives a
	// chunk whose rows keep changing before it reports the chunk unsettled.
	DefaultSettleAttempts = 3
	// DefaultSettleWait is the longest a live pass waits for the catch-up
	// to confirm past the position a chunk was read at.
	DefaultSettleWait = 30 * time.Second
	// DefaultSettlePoll is how often a live pass asks the catch-up for its
	// confirmed position while it waits.
	DefaultSettlePoll = 100 * time.Millisecond
)

// LiveOptions bounds how a live pass settles a chunk that differed while
// the source is written and the catch-up applies the stream. Zero values
// take the defaults; Confirmed has none and is required. Attempts and
// SettleWait bound each chunk, not the pass: a pass over many differing
// chunks can spend them on every one, so a caller with a deadline of its
// own passes it on the context.
type LiveOptions struct {
	// Confirmed is the catch-up applying the stream to the shadow.
	Confirmed ConfirmedSource
	// Attempts is how many fresh rereads a chunk that keeps changing gets
	// before the pass reports it unsettled.
	Attempts int
	// SettleWait bounds each wait for the catch-up to confirm past the
	// position a chunk was read at. It is spent in sleeps of Poll, so it
	// must be at least one Poll long.
	SettleWait time.Duration
	// Poll is the interval between reads of the catch-up's confirmed
	// position during a wait.
	Poll time.Duration
	// Sleep waits between polls; nil waits on the wall clock. Tests inject
	// one to drive the settling without real time passing.
	Sleep func(ctx context.Context, d time.Duration) error
}

// polls is how many sleeps of Poll a wait for the catch-up makes before
// it gives up: the whole of SettleWait, in Poll steps.
func (o LiveOptions) polls() int {
	return int(o.SettleWait / o.Poll)
}

func (o LiveOptions) withDefaults() LiveOptions {
	if o.Attempts == 0 {
		o.Attempts = DefaultSettleAttempts
	}
	if o.SettleWait == 0 {
		o.SettleWait = DefaultSettleWait
	}
	if o.Poll == 0 {
		o.Poll = DefaultSettlePoll
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	return o
}

// validate runs after withDefaults, so every field is set.
func (o LiveOptions) validate() error {
	if o.Confirmed == nil {
		return fmt.Errorf("%w: a live pass needs the catch-up's confirmed position", ErrInvalidOptions)
	}
	if o.Attempts < 1 {
		return fmt.Errorf("%w: settle attempts %d is below one; use zero for the default", ErrInvalidOptions, o.Attempts)
	}
	for _, wait := range []struct {
		name  string
		value time.Duration
	}{{"settle wait", o.SettleWait}, {"settle poll", o.Poll}} {
		if wait.value < time.Millisecond {
			return fmt.Errorf("%w: %s %s is below a millisecond; use zero for the default", ErrInvalidOptions, wait.name, wait.value)
		}
	}
	if o.SettleWait < o.Poll {
		return fmt.Errorf("%w: settle wait %s is shorter than settle poll %s, so no poll would fit in it", ErrInvalidOptions, o.SettleWait, o.Poll)
	}
	return nil
}

// sleepContext waits for d or until ctx ends, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// CheckLive runs one verification pass through the copier's landed
// watermark while the source is written and the catch-up applies the
// stream to the shadow, and acts on what it finds as policy says. The
// sweep is the same as Check's: every chunk cut and digested on both sides
// in one snapshot. A chunk that differs in the sweep is not yet a finding,
// since the shadow may simply not have received the source's latest
// writes: the pass writes a marker to the WAL in the chunk's transaction,
// waits for the catch-up to confirm past it, reads the chunk again in a
// fresh snapshot, and judges the two reads together (settle). A chunk whose
// two sides then agree, or whose shadow has reached exactly what the sweep
// saw on the source, is settled and counts as clean; a chunk that kept
// changing is reread up to LiveOptions.Attempts times and then left
// unsettled; a chunk that held still on both sides across a confirmed
// position and still differs is a divergence. The sweep's premise is that
// the stream is the shadow's only writer, so the shadow either lags the
// source or has caught up to it, and nothing else moves it.
//
// Every differing chunk is settled before the pass decides anything, so
// one chunk that will not settle does not hide a divergence in another;
// the Report counts the unsettled chunks and lists the confirmed
// divergences. A clean pass — every chunk equal or settled — mints the
// proofs as Check does, marked as a live pass's. A pass with unsettled
// chunks and no divergence returns the first unsettled chunk as an
// *UnresolvedError: not a finding about the data, the shadow is as the pass
// found it, and the caller runs the pass again. Under DivergenceAbort a
// divergence is returned as a *DivergenceError with the shadow untouched,
// whether or not other chunks were unsettled. Under DivergenceRepair every
// divergent chunk is recopied from the source in one transaction that
// fences the stream's flushes out of the shadow while it runs, the pass
// waits for the catch-up to confirm past the recopy, and the chunk is
// settled again; a chunk divergent after its repair is a *RepairError, and
// a pass whose repairs all took but which left other chunks unsettled
// returns the first of them as an *UnresolvedError with the repairs
// committed and listed in the Outcome. A pass with repairs mints no proof
// (CO-2). DivergenceRecopy is refused: it is the reconcile mode's policy
// for a pass that reads nothing again, and a live pass settles every chunk
// it recopies.
//
// While it runs the verifier is the tracker's work source: the sweep's
// differing chunks count as chunks_mismatched, and every settling reread
// as chunks_reread. A CheckLive that overlaps a pass still running on this
// verifier is refused with ErrPassRunning.
func (v *Verifier) CheckLive(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, policy DivergencePolicy, live LiveOptions) (Outcome, error) {
	if err := policy.validate(); err != nil {
		return Outcome{}, v.verifyError(err)
	}
	// INV: CO-3
	if policy == DivergenceRecopy {
		return Outcome{}, v.verifyError(fmt.Errorf("%w: %q reads nothing again; a live pass settles every chunk it recopies, so it takes %q or %q", ErrNoDivergencePolicy, DivergenceRecopy, DivergenceAbort, DivergenceRepair))
	}
	live = live.withDefaults()
	if err := live.validate(); err != nil {
		return Outcome{}, v.verifyError(err)
	}
	stop, err := v.report()
	if err != nil {
		return Outcome{}, v.verifyError(err)
	}
	defer stop()
	if !through.Valid() {
		return Outcome{}, fmt.Errorf("%w: %s.%s", ErrNothingLanded, v.target.Schema(), v.target.Table())
	}
	ctx, unbind := v.lock.Bind(ctx)
	defer unbind()
	report, unresolved, err := v.sweepAndSettle(ctx, pool, through, live)
	if err != nil {
		return Outcome{}, v.lostOr(err)
	}
	if report.Clean() {
		return v.prove(report, true), nil
	}
	if len(report.Mismatches) == 0 {
		return Outcome{Report: report}, v.verifyError(unresolved)
	}
	// INV: CO-3
	switch policy {
	case DivergenceAbort:
		return Outcome{Report: report}, v.verifyError(&DivergenceError{Report: report})
	case DivergenceRepair:
		repairs, err := v.repairLive(ctx, pool, report.Mismatches, live)
		if err != nil {
			return Outcome{Report: report, Repairs: repairs}, v.verifyError(v.lostOr(err))
		}
		if unresolved != nil {
			return Outcome{Report: report, Repairs: repairs}, v.verifyError(unresolved)
		}
		return Outcome{Report: report, Repairs: repairs}, nil
	default:
		return Outcome{Report: report}, fmt.Errorf("%w (CO-3): divergence policy %q passed validation", ErrInvariantViolation, string(policy))
	}
}

// sweepAndSettle runs the live sweep and settles every chunk it found
// differing, in key order, returning the report with the confirmed
// divergences as its mismatches and the settled and unsettled chunks
// counted, and the first unsettled chunk when there was one. A chunk that
// does not settle is counted and the next is settled; only a failure that
// says nothing about the data — the context ending, the lock lost, a read
// refused — stops the settling.
func (v *Verifier) sweepAndSettle(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, live LiveOptions) (Report, *UnresolvedError, error) {
	sourceSQL, shadowSQL, err := v.digestStatements(ctx, pool)
	if err != nil {
		return Report{}, nil, err
	}
	report, differing, err := v.sweep(ctx, pool, sourceSQL, shadowSQL, through, true)
	if err != nil {
		return Report{}, nil, err
	}
	var first *UnresolvedError
	for _, prior := range differing {
		verdict, now, err := v.settle(ctx, pool, sourceSQL, shadowSQL, prior, 0, live)
		var unresolved *UnresolvedError
		if errors.As(err, &unresolved) {
			report.Unsettled++
			if first == nil {
				first = unresolved
			}
			continue
		}
		if err != nil {
			return Report{}, nil, err
		}
		switch verdict {
		case verdictEqual, verdictSettled:
			report.Settled++
		case verdictDiverged:
			report.Mismatches = append(report.Mismatches, Mismatch{Chunk: now.chunk, Source: now.source, Shadow: now.shadow})
		default:
			return Report{}, nil, fmt.Errorf("%w (CO-1): settle returned verdict %d", ErrInvariantViolation, verdict)
		}
	}
	if lost := v.lockLost(); lost != nil {
		return Report{}, nil, lost
	}
	return report, first, nil
}
