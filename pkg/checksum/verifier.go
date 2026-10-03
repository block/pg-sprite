package checksum

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/internal/chunksql"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
)

var (
	// ErrInvariantViolation marks a fail-closed refusal; the wrapped message
	// names the invariant.
	ErrInvariantViolation = dbconn.ErrInvariantViolation
	// ErrInvalidOptions reports verifier options that cannot bound a pass: a
	// timeout below PostgreSQL's one-millisecond resolution, which the server
	// would read as no timeout at all.
	ErrInvalidOptions = errors.New("invalid verifier options")
	// ErrNothingLanded reports a pass asked to verify up to the zero
	// watermark, below which no key has been copied: there is nothing to
	// compare, and a pass that compared nothing must not read as clean.
	ErrNothingLanded = errors.New("nothing has landed to verify")
)

// Options bounds a verification pass. Zero values take the defaults: the
// dbconn session timeouts for every chunk transaction, the copier's
// ChunkerOptions defaults, and the wall clock.
type Options struct {
	// LockTimeout bounds every lock wait inside a chunk transaction.
	LockTimeout time.Duration
	// StatementTimeout bounds every statement inside a chunk transaction.
	StatementTimeout time.Duration
	// Chunker sizes the chunks a pass compares. They are cut independently
	// of the chunks the copier copied; only the key range matters.
	Chunker copier.ChunkerOptions
	// Clock times each chunk for the chunker's chunk-time sizing feedback
	// (docs/copy-and-swap-design.md#d12--throttle-by-chunk-time-and-slot-lag).
	Clock progress.Clock
}

func (o Options) withDefaults() Options {
	if o.LockTimeout == 0 {
		o.LockTimeout = dbconn.DefaultLockTimeout
	}
	if o.StatementTimeout == 0 {
		o.StatementTimeout = dbconn.DefaultStatementTimeout
	}
	if o.Clock == nil {
		o.Clock = progress.WallClock{}
	}
	return o
}

// validate runs after withDefaults, so every field is set.
func (o Options) validate() error {
	for _, timeout := range []struct {
		name  string
		value time.Duration
	}{{"lock timeout", o.LockTimeout}, {"statement timeout", o.StatementTimeout}} {
		if timeout.value < time.Millisecond {
			// INV: LK-2
			return fmt.Errorf("%w: %s %s is below PostgreSQL's one-millisecond resolution; use zero for the default", ErrInvalidOptions, timeout.name, timeout.value)
		}
	}
	return nil
}

// Verifier compares a proven source table with its built shadow, chunk by
// chunk, and reports every chunk whose rows differ. Each chunk is read in
// its own short read-only transaction whose one snapshot covers both
// tables, so a chunk's two digests describe the same instant and no pass
// holds a snapshot open for longer than one chunk. It runs only under the
// table's lock session. A Verifier holds no state between passes; the same
// one can run a pass after every repair.
type Verifier struct {
	target preflight.CopySwapTarget
	shadow copier.Shadow
	lock   *dbconn.TableLockSession
	opts   Options
	// repairSQL clears one chunk of the shadow before its recopy and
	// copySQL puts the source's rows back, the chunk insert the copier
	// runs; both are frozen at construction so no pass builds SQL.
	repairSQL string
	copySQL   string
}

// NewVerifier prepares verification of target against shadow. It refuses a
// proof, shadow, or lock session that does not describe this table, and
// options that cannot bound a pass. The shadow is checked as the copier
// checks it: the verifier reads both relations the copier wrote between and
// must refuse everything the copier would have.
func NewVerifier(target preflight.CopySwapTarget, shadow copier.Shadow, lock *dbconn.TableLockSession, opts Options) (*Verifier, error) {
	// INV: ST-6
	if target.Table() == "" {
		return nil, fmt.Errorf("%w (ST-6): copy-and-swap target proof is empty", ErrInvariantViolation)
	}
	if err := checkShadow(target, shadow); err != nil {
		return nil, err
	}
	if err := requireTableLock(lock, target); err != nil {
		return nil, err
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	// The chunker validates its own options; build one now so a pass never
	// starts with options that cannot cut.
	if _, err := copier.NewChunker(target, copier.Watermark{}, opts.Chunker); err != nil {
		return nil, err
	}
	return &Verifier{
		target: target, shadow: shadow, lock: lock, opts: opts,
		repairSQL: repairSQL(target, shadow),
		copySQL:   chunksql.Insert(shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), target.PKColumn(), shadow.CopyColumns()),
	}, nil
}

// checkShadow refuses a shadow proof that does not describe the proven
// target's shadow, with the copier's rules: the verifier compares the two
// relations the copier copied between, so a proof the copier would refuse
// describes nothing the verifier can vouch for.
func checkShadow(target preflight.CopySwapTarget, shadow copier.Shadow) error {
	// INV: ST-6
	if shadow == nil {
		return fmt.Errorf("%w (ST-6): verification requires a built shadow", ErrInvariantViolation)
	}
	if shadow.ShadowTable() == "" {
		return fmt.Errorf("%w (ST-6): shadow proof is empty", ErrInvariantViolation)
	}
	if shadow.Schema() != target.Schema() || shadow.SourceTable() != target.Table() {
		return fmt.Errorf("%w (ST-6): shadow is for %s.%s, proof is for %s.%s", ErrInvariantViolation, shadow.Schema(), shadow.SourceTable(), target.Schema(), target.Table())
	}
	if shadow.ShadowTable() == shadow.SourceTable() {
		return fmt.Errorf("%w (ST-6): shadow of %s.%s is the source table itself", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if shadow.SourceOID() == 0 || shadow.ShadowOID() == 0 {
		return fmt.Errorf("%w (ST-6): shadow proof for %s.%s carries no relation OIDs", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if shadow.SourceOID() == shadow.ShadowOID() {
		return fmt.Errorf("%w (ST-6): shadow proof for %s.%s names relation %d as both source and shadow", ErrInvariantViolation, target.Schema(), target.Table(), shadow.SourceOID())
	}
	if !slices.Contains(shadow.CopyColumns(), target.PKColumn()) {
		return fmt.Errorf("%w (ST-6): shadow copy columns for %s.%s do not include the primary key %s", ErrInvariantViolation, target.Schema(), target.Table(), target.PKColumn())
	}
	return nil
}

// requireTableLock refuses to verify without the per-table lock that keeps
// a second engine instance off the same table: the session must exist,
// carry a populated proof for the proven table, and not have reported loss.
func requireTableLock(lock *dbconn.TableLockSession, target preflight.CopySwapTarget) error {
	// INV: LK-1
	if lock == nil {
		return fmt.Errorf("%w (LK-1): verification requires a table lock session", ErrInvariantViolation)
	}
	held := lock.Lock()
	if held.Table() == "" {
		return fmt.Errorf("%w (LK-1): table lock proof is empty", ErrInvariantViolation)
	}
	if held.Schema() != target.Schema() || held.Table() != target.Table() {
		return fmt.Errorf("%w (LK-1): table lock is for %s.%s, proof is for %s.%s", ErrInvariantViolation, held.Schema(), held.Table(), target.Schema(), target.Table())
	}
	if err := lock.Err(); err != nil {
		return fmt.Errorf("%w (LK-1): table lock was lost before verification: %w", ErrInvariantViolation, err)
	}
	return nil
}

// Verify runs one pass over every key at or below through — the copier's
// landed watermark, below which every chunk has committed — and reports
// what it found. Keys above the watermark are not compared: they are in
// chunks the copier has not finished, and a difference there is expected,
// not a finding. The pass reads the shadow's column types once, then cuts
// and digests each chunk in one guarded transaction of its own, so the cut
// and both digests read one snapshot and the cut's lock wait is bounded
// like the reads. It stops at the first error; a report with mismatches is not
// an error, and the caller's divergence policy decides what to do with it.
// Every transaction runs under the lock session's Bind context, so losing
// the table lock cancels the read in flight and Verify reports the loss.
func (v *Verifier) Verify(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark) (Report, error) {
	if !through.Valid() {
		return Report{}, fmt.Errorf("%w: %s.%s", ErrNothingLanded, v.target.Schema(), v.target.Table())
	}
	ctx, unbind := v.lock.Bind(ctx)
	defer unbind()
	report, err := v.pass(ctx, pool, through)
	if err != nil {
		return Report{}, v.lostOr(err)
	}
	if lost := v.lockLost(); lost != nil {
		return Report{}, lost
	}
	return report, nil
}

// lockLost reports the table lock's loss as the verifier's invariant
// violation, or nil while the session still holds it.
func (v *Verifier) lockLost() error {
	lost := v.lock.Err()
	if lost == nil {
		return nil
	}
	// INV: LK-1
	return fmt.Errorf("%w (LK-1): table lock was lost during verification: %w", ErrInvariantViolation, lost)
}

// lostOr returns the lock's loss when the session has reported one — a read
// cancelled by Bind is explained by the loss, not by its own error — and err
// otherwise.
func (v *Verifier) lostOr(err error) error {
	if lost := v.lockLost(); lost != nil {
		return lost
	}
	return err
}

func (v *Verifier) pass(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark) (Report, error) {
	sourceSQL, shadowSQL, err := v.digestStatements(ctx, pool)
	if err != nil {
		return Report{}, err
	}
	chunker, err := copier.NewChunker(v.target, copier.Watermark{}, v.opts.Chunker)
	if err != nil {
		return Report{}, err
	}
	report := Report{Through: through}
	for {
		started := v.opts.Clock.Now()
		next, ok, err := v.compareNext(ctx, pool, chunker, sourceSQL, shadowSQL, through)
		if err != nil {
			return Report{}, err
		}
		if !ok {
			return report, nil
		}
		report.Chunks++
		report.Rows += next.source.Rows
		if next.source != next.shadow {
			report.Mismatches = append(report.Mismatches, Mismatch{Chunk: next.compared, Source: next.source, Shadow: next.shadow})
		}
		if next.compared.Upper() == through.Value() {
			return report, nil
		}
		if err := chunker.Feedback(next.cut, v.opts.Clock.Now().Sub(started)); err != nil {
			return Report{}, err
		}
	}
}

// comparison is one chunk of a pass: the chunk the chunker cut, the closed
// range the pass compared — that chunk capped at the pass's watermark — and
// the two digests of that range.
type comparison struct {
	cut      copier.Chunk
	compared copier.Chunk
	source   Digest
	shadow   Digest
}

// digestStatements freezes the two digest statements for this pass from the
// shadow's column types, read in a guarded transaction so the shadow whose
// types they name is the shadow the proof describes.
func (v *Verifier) digestStatements(ctx context.Context, pool *pgxpool.Pool) (sourceSQL, shadowSQL string, err error) {
	tx, err := v.begin(ctx, pool, snapshotRead())
	if err != nil {
		return "", "", err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	types, err := shadowColumnTypes(ctx, tx, v.shadow)
	if err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("commit column-type read of %s.%s: %w", v.shadow.Schema(), v.shadow.ShadowTable(), err)
	}
	return digestSQL(v.target, v.shadow.Schema(), v.shadow.SourceTable(), types),
		digestSQL(v.target, v.shadow.Schema(), v.shadow.ShadowTable(), types),
		nil
}

// compareNext cuts the next chunk from the source and reads both sides of
// it in one guarded transaction, so the cut and the two digests describe
// one snapshot: the chunk holds exactly its row count as the digests saw
// the table. The cut runs after the guard has bounded the transaction and
// locked both relations, so a cut queued behind a strong lock on the source
// ends at the verifier's lock timeout whatever the caller's pool carries
// (LK-2). ok is false once the key space is covered.
func (v *Verifier) compareNext(ctx context.Context, pool *pgxpool.Pool, chunker *copier.Chunker, sourceSQL, shadowSQL string, through copier.Watermark) (comparison, bool, error) {
	tx, err := v.begin(ctx, pool, snapshotRead())
	if err != nil {
		return comparison{}, false, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	cut, ok, err := chunker.Next(ctx, tx)
	if err != nil {
		return comparison{}, false, err
	}
	if !ok {
		if err := tx.Commit(ctx); err != nil {
			return comparison{}, false, fmt.Errorf("commit final cut of %s.%s: %w", v.target.Schema(), v.target.Table(), err)
		}
		return comparison{}, false, nil
	}
	lower, upper := cut.Lower(), min(cut.Upper(), through.Value())
	source, shadow, err := v.digestRange(ctx, tx, sourceSQL, shadowSQL, lower, upper)
	if err != nil {
		return comparison{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return comparison{}, false, fmt.Errorf("commit digest of chunk [%d, %d] of %s.%s: %w", lower, upper, v.target.Schema(), v.target.Table(), err)
	}
	compared, err := copier.NewChunk(lower, upper)
	if err != nil {
		return comparison{}, false, fmt.Errorf("%w (CO-1): verified chunk: %w", ErrInvariantViolation, err)
	}
	return comparison{cut: cut, compared: compared, source: source, shadow: shadow}, true, nil
}

// digestChunk reads both sides of the closed range [lower, upper] in one
// guarded transaction of its own, so the two digests describe one snapshot.
func (v *Verifier) digestChunk(ctx context.Context, pool *pgxpool.Pool, sourceSQL, shadowSQL string, lower, upper int64) (source, shadow Digest, err error) {
	tx, err := v.begin(ctx, pool, snapshotRead())
	if err != nil {
		return Digest{}, Digest{}, err
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	source, shadow, err = v.digestRange(ctx, tx, sourceSQL, shadowSQL, lower, upper)
	if err != nil {
		return Digest{}, Digest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Digest{}, Digest{}, fmt.Errorf("commit digest of chunk [%d, %d] of %s.%s: %w", lower, upper, v.target.Schema(), v.target.Table(), err)
	}
	return source, shadow, nil
}

// digestRange reads both sides of the closed range [lower, upper] in tx.
func (v *Verifier) digestRange(ctx context.Context, tx pgx.Tx, sourceSQL, shadowSQL string, lower, upper int64) (source, shadow Digest, err error) {
	source, err = digest(ctx, tx, sourceSQL, lower, upper)
	if err != nil {
		return Digest{}, Digest{}, fmt.Errorf("digest chunk [%d, %d] of %s.%s: %w", lower, upper, v.shadow.Schema(), v.shadow.SourceTable(), err)
	}
	shadow, err = digest(ctx, tx, shadowSQL, lower, upper)
	if err != nil {
		return Digest{}, Digest{}, fmt.Errorf("digest chunk [%d, %d] of shadow %s.%s: %w", lower, upper, v.shadow.Schema(), v.shadow.ShadowTable(), err)
	}
	return source, shadow, nil
}
