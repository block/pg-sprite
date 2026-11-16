package change

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
)

// Defaults for the LogicalConfig bounds a caller leaves zero.
const (
	defaultReadWait     = time.Second
	defaultWaitTimeout  = 30 * time.Second
	defaultCloseTimeout = 10 * time.Second
)

// errNotStarted is returned by a wait on a source whose reader has not been
// started.
var errNotStarted = errors.New("change: the source has not been started")

// errReaderStopped is returned by a wait whose reader exited without an
// error of its own: the source was closed or its context ended.
var errReaderStopped = errors.New("change: the source's reader has stopped")

// SubscriptionFactory builds the subscription AddSubscription registers.
type SubscriptionFactory func(currentTable, newTable *TableInfo, chunker MappedChunker) (Subscription, error)

// LogicalConfig configures a LogicalSource.
type LogicalConfig struct {
	// Conn is the source database's connection configuration; the
	// replication connection is dialed from it.
	Conn dbconn.Config
	// Pool is a pool on the source database, for catalog reads and WAL
	// markers.
	Pool *pgxpool.Pool
	// Target is the table streamed, proven for a run that decodes WAL. Its
	// slot and publication are the ones the source reads.
	Target preflight.CopySwapTarget
	// NewSubscription builds the subscription for AddSubscription.
	NewSubscription SubscriptionFactory
	// OnFatal, when set, is called once with the error that ended the
	// stream. Flush, BlockWait, and VerifyRowAtNextChange return it too.
	OnFatal func(error)
	// Logger receives diagnostics; nil discards them.
	Logger *slog.Logger
	// Clock times flushes; nil is the wall clock.
	Clock progress.Clock
	// ReadWait bounds each read of the stream, and so how long a flushed
	// position waits to be confirmed to the slot. Zero is one second.
	ReadWait time.Duration
	// WaitTimeout bounds BlockWait and a verification's wait for a change.
	// Zero is 30 seconds.
	WaitTimeout time.Duration
	// CloseTimeout bounds closing the replication connection. Zero is 10
	// seconds.
	CloseTimeout time.Duration
}

func (c LogicalConfig) withDefaults() LogicalConfig {
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Clock == nil {
		c.Clock = progress.WallClock{}
	}
	if c.ReadWait == 0 {
		c.ReadWait = defaultReadWait
	}
	if c.WaitTimeout == 0 {
		c.WaitTimeout = defaultWaitTimeout
	}
	if c.CloseTimeout == 0 {
		c.CloseTimeout = defaultCloseTimeout
	}
	return c
}

func (c LogicalConfig) validate() error {
	if c.Pool == nil {
		return errors.New("change: LogicalConfig.Pool is required")
	}
	if c.Target.Table() == "" {
		return errors.New("change: LogicalConfig.Target is required")
	}
	if !c.Target.DecodesWAL() {
		return fmt.Errorf("change: target %s.%s was not verified for a run that decodes WAL", c.Target.Schema(), c.Target.Table())
	}
	if c.NewSubscription == nil {
		return errors.New("change: LogicalConfig.NewSubscription is required")
	}
	if c.ReadWait < 0 || c.WaitTimeout < 0 || c.CloseTimeout < 0 {
		return errors.New("change: LogicalConfig durations must not be negative")
	}
	return nil
}

// LogicalSource is a Source over the logical decoding of one copy-and-swap
// target's slot. It takes one subscription, for the target's table, whose
// key is the target's single integer primary-key column.
//
// One reader goroutine owns the decode.Stream: it reads, hands each change
// to the subscription, and confirms the flushed position to the slot.
// Flushes run on their caller's goroutine, one at a time.
type LogicalSource struct {
	cfg LogicalConfig

	// mu guards every field below it.
	mu sync.Mutex
	// sub and table are set by AddSubscription before the reader starts and
	// never change after, so the reader reads them without mu.
	sub   Subscription
	table *TableInfo
	// immutableKey is true when the subscription declares the key column
	// immutable, so a key-moving UPDATE is fatal.
	immutableKey bool
	started      bool
	closed       bool
	// buffered is the position every change below which has been handed to
	// the subscription; flushed is the position every change below which
	// the subscription has flushed, the resume position.
	buffered decode.LSN
	flushed  decode.LSN
	// advanced is closed and replaced whenever buffered moves.
	advanced chan struct{}
	fatal    error
	// stopReader ends the reader; readerDone closes when it has returned.
	stopReader context.CancelFunc
	readerDone chan struct{}
	reader     sync.WaitGroup

	flushResidual     int
	flushCount        int
	lastFlushAt       time.Time
	lastFlushDuration time.Duration
	lastFlushRows     int

	// flushMu serializes flushes.
	flushMu sync.Mutex
	// received counts changes the reader has read and dispatched those it
	// has finished handing to the subscription.
	received   atomic.Int64
	dispatched atomic.Int64
	stopped    atomic.Bool

	periodic periodicFlush
	parker   rowParker
}

// The compile-time proof that LogicalSource has the Source method set.
var _ Source = (*LogicalSource)(nil)

// NewLogicalSource returns a source for cfg's target. Nothing is read from
// the server until Start or StartFromPosition.
func NewLogicalSource(cfg LogicalConfig) (*LogicalSource, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &LogicalSource{cfg: cfg.withDefaults(), advanced: make(chan struct{})}, nil
}

// AddSubscription builds the subscription for the target's table and
// registers it. currentTable must name the target, and its key must be the
// target's primary-key column. A subscription whose immutable column is not
// the key is refused: the stream does not carry a row's prior values, so a
// change to that column could not be detected.
func (s *LogicalSource) AddSubscription(currentTable, newTable *TableInfo, chunker MappedChunker) error {
	if err := s.checkTable(currentTable); err != nil {
		return err
	}
	sub, err := s.cfg.NewSubscription(currentTable, newTable, chunker)
	if err != nil {
		return fmt.Errorf("build subscription for %s.%s: %w", currentTable.SchemaName, currentTable.TableName, err)
	}
	immutableKey, err := immutableIsKey(sub, currentTable)
	if err != nil {
		sub.Close()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		sub.Close()
		return errors.New("change: AddSubscription after Start")
	}
	if s.sub != nil {
		sub.Close()
		return fmt.Errorf("change: %s.%s already has a subscription; the stream carries one table", currentTable.SchemaName, currentTable.TableName)
	}
	s.sub, s.table, s.immutableKey = sub, currentTable, immutableKey
	return nil
}

// checkTable checks that a subscription's table is the target's.
func (s *LogicalSource) checkTable(tbl *TableInfo) error {
	target := s.cfg.Target
	if tbl == nil {
		return errors.New("change: AddSubscription requires currentTable")
	}
	if tbl.SchemaName != target.Schema() || tbl.TableName != target.Table() {
		return fmt.Errorf("change: subscription for %s.%s, the stream carries %s.%s",
			tbl.SchemaName, tbl.TableName, target.Schema(), target.Table())
	}
	if !slices.Equal(tbl.KeyColumns, []string{target.PKColumn()}) {
		return fmt.Errorf("change: subscription for %s.%s keys on %v, the table's key is %s",
			tbl.SchemaName, tbl.TableName, tbl.KeyColumns, target.PKColumn())
	}
	if !slices.Contains(tbl.Columns, target.PKColumn()) {
		return fmt.Errorf("change: subscription for %s.%s lists no column %s", tbl.SchemaName, tbl.TableName, target.PKColumn())
	}
	return nil
}

// immutableIsKey reports whether the subscription declares the key column
// immutable, and refuses one that declares any other column so.
func immutableIsKey(sub Subscription, tbl *TableInfo) (bool, error) {
	ordinal := sub.ImmutableColumnOrdinal()
	if ordinal < 0 {
		return false, nil
	}
	if ordinal >= len(tbl.Columns) {
		return false, fmt.Errorf("%w: immutable column ordinal %d of %s.%s, which has %d columns",
			ErrInvariantViolation, ordinal, tbl.SchemaName, tbl.TableName, len(tbl.Columns))
	}
	if tbl.Columns[ordinal] != tbl.KeyColumns[0] {
		return false, fmt.Errorf("change: %s.%s declares column %s immutable; only the key column can be checked",
			tbl.SchemaName, tbl.TableName, tbl.Columns[ordinal])
	}
	return true, nil
}

// Start creates the target's slot and streams from its consistent point:
// every change committed after the slot was created is delivered. A slot of
// the target's name that already exists is refused (decode.SlotExistsError);
// resume from it with StartFromPosition, or drop it. Close leaves the slot
// in place.
func (s *LogicalSource) Start(ctx context.Context) error {
	if err := s.claimStart(); err != nil {
		return err
	}
	slot, err := decode.CreateSlot(ctx, s.cfg.Conn, s.cfg.Pool, s.cfg.Target)
	if err != nil {
		s.releaseStart()
		return fmt.Errorf("start change source for %s.%s: %w", s.cfg.Target.Schema(), s.cfg.Target.Table(), err)
	}
	from := slot.ConsistentPoint()
	if err := slot.Close(ctx); err != nil {
		s.releaseStart()
		return fmt.Errorf("start change source for %s.%s: %w", s.cfg.Target.Schema(), s.cfg.Target.Table(), err)
	}
	return s.open(ctx, from)
}

// StartFromPosition streams the target's existing slot from pos. A slot
// that no longer exists or has lost its WAL is ErrPositionNotFound.
func (s *LogicalSource) StartFromPosition(ctx context.Context, pos string) error {
	from, err := parsePosition(pos)
	if err != nil {
		return err
	}
	if err := s.claimStart(); err != nil {
		return err
	}
	if err := requireResumable(ctx, s.cfg.Pool, s.cfg.Target.DecodingName(), from); err != nil {
		s.releaseStart()
		return err
	}
	return s.open(ctx, from)
}

// claimStart marks the source started, once, with a subscription in place.
func (s *LogicalSource) claimStart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return errors.New("change: Start after Close")
	case s.started:
		return errors.New("change: the source is already started")
	case s.sub == nil:
		return errors.New("change: Start before AddSubscription")
	}
	s.started = true
	return nil
}

// releaseStart undoes claimStart after a start that failed.
func (s *LogicalSource) releaseStart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = false
}

// open starts the stream at from and the reader that owns it. Everything
// below from is treated as flushed: on a fresh start nothing below the
// consistent point is owed, and a resume starts at a flushed position.
func (s *LogicalSource) open(ctx context.Context, from decode.LSN) error {
	stream, err := decode.OpenStream(ctx, s.cfg.Conn, s.cfg.Pool, s.cfg.Target, from)
	if err != nil {
		s.releaseStart()
		return fmt.Errorf("start change source for %s.%s: %w", s.cfg.Target.Schema(), s.cfg.Target.Table(), err)
	}
	readerCtx, stop := context.WithCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		stop()
		s.closeStream(ctx, stream)
		return errors.New("change: the source was closed while starting")
	}
	s.buffered, s.flushed = from, from
	s.stopReader = stop
	s.readerDone = make(chan struct{})
	done := s.readerDone
	s.reader.Go(func() {
		defer close(done)
		s.read(readerCtx, stream)
	})
	return nil
}

// Position is the flushed position: every change committed below it has
// been flushed by the subscription.
func (s *LogicalSource) Position() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return formatPosition(s.flushed)
}

// CurrentPosition is the server's current WAL write position.
func (s *LogicalSource) CurrentPosition(ctx context.Context) (string, error) {
	var text string
	if err := s.cfg.Pool.QueryRow(ctx, `SELECT pg_catalog.pg_current_wal_lsn()::text`).Scan(&text); err != nil {
		return "", fmt.Errorf("read the current WAL position: %w", err)
	}
	lsn, err := decode.ParseLSN(text)
	if err != nil {
		return "", fmt.Errorf("read the current WAL position: %w", err)
	}
	return formatPosition(lsn), nil
}

// Stop ends delivery to the subscription. The reader keeps reading and the
// position keeps moving, so the source stays flushable.
func (s *LogicalSource) Stop() {
	s.stopped.Store(true)
}

// Close stops any periodic flush and the reader, releases a reader held by
// the subscription's backpressure or by a verification, closes the
// replication connection, and closes the subscription. The slot persists,
// for a resume or for the caller to drop.
func (s *LogicalSource) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	stop, sub := s.stopReader, s.sub
	s.mu.Unlock()

	s.StopPeriodicFlush()
	if stop != nil {
		stop()
	}
	s.parker.releaseAll()
	if sub != nil {
		sub.Close()
	}
	s.reader.Wait()
}

// GetDeltaLen is the number of changes the subscription holds.
func (s *LogicalSource) GetDeltaLen() int {
	s.mu.Lock()
	sub := s.sub
	s.mu.Unlock()
	if sub == nil {
		return 0
	}
	return sub.Length()
}

// SetWatermarkOptimization forwards to the subscription.
func (s *LogicalSource) SetWatermarkOptimization(ctx context.Context, enabled bool) error {
	s.mu.Lock()
	sub := s.sub
	s.mu.Unlock()
	if sub == nil {
		return nil
	}
	return sub.SetWatermarkOptimization(ctx, enabled)
}

// AllChangesFlushed reports whether every change read has been handed to the
// subscription and the subscription holds none.
func (s *LogicalSource) AllChangesFlushed() bool {
	if s.dispatched.Load() < s.received.Load() {
		return false
	}
	return s.GetDeltaLen() == 0
}

// FeedStats reports the last flush and how far the reader has read. The
// stream reports no commit times, rotations, or parks.
func (s *LogicalSource) FeedStats() FeedStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return FeedStats{
		LastFlushAt:       s.lastFlushAt,
		LastFlushDuration: s.lastFlushDuration,
		LastFlushRows:     s.lastFlushRows,
		BufferedPosition:  formatPosition(s.buffered),
	}
}

// FlushResidual reports what the last completed flush left behind and how
// many flushes have completed.
func (s *LogicalSource) FlushResidual() (residual, flushes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushResidual, s.flushCount
}
