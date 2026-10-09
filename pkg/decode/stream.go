package decode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// ErrUnsupportedChange is returned when the stream decodes a change to the
// target table that the route does not replay row by row — a TRUNCATE. The
// shadow cannot be brought back into step by applying rows, so the stream
// stops and the route decides whether to restart the copy.
var ErrUnsupportedChange = errors.New("the source table received a change the route does not replay")

// ErrStreamEnded is returned when the server ends replication in an orderly
// way — a shutdown with the slot intact — rather than with an error. This
// is the clean resume ST-4 separates from slot loss: the slot survives with
// a position at or below the last confirm (a confirm that only moves the
// slot's confirmed position is not always written out before a shutdown),
// and a new stream started from the caller's own checkpoint continues where
// this one stopped, since the server forwards a start that lies below the
// slot's position.
var ErrStreamEnded = errors.New("the server ended the replication stream")

// protocolVersion is the pgoutput protocol the stream speaks: version 1,
// which every supported server offers, without streaming of in-progress
// transactions, so every change the stream yields has already committed.
const protocolVersion = "1"

// Delivery is what one call to Stream.Next yields: a decoded change, or
// nothing but a position when the server reported progress without a change
// — a transaction's commit, or a keepalive while the table is quiet.
type Delivery struct {
	// Change is the decoded row change; nil for a progress-only delivery.
	Change *ChangeEvent
	// Delivered is the stream's position after this delivery: every
	// transaction that committed at or below it has been yielded in full,
	// so it is the ceiling of what the caller may confirm. A change's own
	// LSN can lie below it, since a transaction is sent when it commits,
	// not when its rows were written. For a change delivery it equals
	// Change.Delivered.
	Delivered LSN
}

// Stream decodes the route's slot into ChangeEvents. It owns one replication
// connection from the start position onwards and keeps two positions: what
// it has delivered to the caller, and what the caller has confirmed applied,
// which is the only position it ever reports to the server (ST-4 — the
// slot's confirmed position is the resume point, so it moves only on the
// caller's word). Both positions order transactions by their commit: a
// transaction is sent whole when it commits, so a change's own LSN can lie
// below either of them.
//
// A Stream is used from one goroutine, which must call Next often enough to
// answer the server's keepalives within wal_sender_timeout. The server's
// fast shutdown waits for the stream to confirm everything it was sent, so
// the caller confirms Delivered whenever it holds nothing unapplied, and
// closes a stream that can neither confirm nor make progress rather than
// keep it open.
type Stream struct {
	conn     *pgconn.PgConn
	slotName string
	target   preflight.CopySwapTarget
	// start is the position decoding was requested from; a transaction
	// that committed above it may carry changes written below it.
	start     LSN
	delivered LSN
	confirmed LSN
	// serverWALEnd is the walsender's send position from its last
	// keepalive; a change does not move it, since a change's XLogData
	// carries only the change's own position.
	serverWALEnd LSN
	// relation is the target table as pgoutput last described it; nil
	// until the first change arrives.
	relation      *relation
	inTransaction bool
	// failed is the error that ended the stream, returned from then on.
	failed error
}

// OpenStream starts decoding the target's slot from a position, on a fresh
// replication connection built from cfg and proven to be a session of the
// pool's cluster on the target's database — a database of the target's
// name on another cluster holds a slot of the same derived name, whose rows
// would otherwise arrive as this target's changes. The position is the
// slot's consistent point for a first run or
// the checkpointed applied position for a resume; the server replays every
// transaction that committed above the slot's confirmed position, so a
// resume sees again what it applied but never confirmed. The exported
// snapshot of a Slot is unaffected, since the slot's own connection is not
// used.
func OpenStream(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, target preflight.CopySwapTarget, from LSN) (*Stream, error) {
	if target.Table() == "" {
		return nil, fmt.Errorf("%w: ST-3: zero copy-and-swap target", ErrInvariantViolation)
	}
	if !target.DecodesWAL() {
		return nil, fmt.Errorf("%w: ST-3: the target's environment was not verified for a run that decodes WAL", ErrInvariantViolation)
	}
	name := target.DecodingName()
	if !slotNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: ST-3: derived name %q is not an engine slot name", ErrInvariantViolation, name)
	}
	if from == 0 {
		return nil, fmt.Errorf("%w: ST-4: stream from slot %s has no start position", ErrInvariantViolation, name)
	}

	conn, err := dbconn.ConnectReplication(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open stream from slot %s: %w", name, err)
	}
	// INV: ST-3 — the slot is named for the target's database on the pool's
	// cluster; a connection anywhere else would decode another database's
	// slot of the same name.
	identity, err := proveSameServer(ctx, conn, pool)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open stream from slot %s: %w", name, err), conn.Close(ctx))
	}
	if identity.database != target.Database() {
		return nil, errors.Join(fmt.Errorf("%w: ST-3: replication connection is on database %q, the target is in %q",
			ErrInvariantViolation, identity.database, target.Database()), conn.Close(ctx))
	}

	err = pglogrepl.StartReplication(ctx, conn, name, pglogrepl.LSN(from), pglogrepl.StartReplicationOptions{
		Mode: pglogrepl.LogicalReplication,
		PluginArgs: []string{
			"proto_version '" + protocolVersion + "'",
			"publication_names " + quoteLiteral(name),
		},
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start replication from slot %s at %s: %w", name, from, err), conn.Close(ctx))
	}
	return &Stream{conn: conn, slotName: name, target: target, start: from, delivered: from}, nil
}

// quoteLiteral renders s as a SQL string literal for a replication command,
// which takes its options as literals rather than parameters.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Slot is the name of the slot the stream decodes, so a caller can inspect
// the slot it is confirming to.
func (s *Stream) Slot() string { return s.slotName }

// Start is the position decoding was requested from. The server never
// decodes from below the slot's confirmed position, so a request below it
// is forwarded there; what the stream was actually sent first shows in the
// first delivery's position.
func (s *Stream) Start() LSN { return s.start }

// Delivered is the position every transaction yielded so far committed at
// or below, and the ceiling of what the caller may confirm.
func (s *Stream) Delivered() LSN { return s.delivered }

// ServerWALEnd is the walsender's send position from its last keepalive —
// the end of the last record it decoded — and never less than Delivered. A
// change does not move it: a change's XLogData carries the change's own
// position, which under commit ordering can lie below Delivered. Delivered
// subtracted from it is a lower bound on the stream's lag, not the lag
// itself: while the walsender works through a backlog, most of the backlog
// is WAL it has not decoded yet, so the figure stays small exactly when the
// stream is furthest behind. Lag against the server's WAL end is measured
// on a pool, against pg_current_wal_lsn(). Delivered until the server
// reports.
func (s *Stream) ServerWALEnd() LSN { return max(s.serverWALEnd, s.delivered) }

// Next yields the next delivery: a decoded change, or a progress-only
// delivery when a transaction commits or the server sends a keepalive. When
// nothing arrives within wait it yields the current position with no
// change, so a quiet table never blocks the caller for longer than wait. A
// keepalive that asks for a reply is answered with the confirmed position,
// never the server's own. An error from the server, a change the stream
// cannot decode, or the server ending replication (ErrStreamEnded) ends the
// stream: the error is returned now and from every later call.
func (s *Stream) Next(ctx context.Context, wait time.Duration) (Delivery, error) {
	if s.failed != nil {
		return Delivery{}, s.failed
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		msg, err := s.conn.ReceiveMessage(waitCtx)
		if err != nil {
			if waitElapsed(ctx, waitCtx, err) {
				return s.progress(), nil
			}
			return Delivery{}, s.fail(fmt.Errorf("receive from slot %s: %w", s.slotName, err))
		}
		delivery, yielded, err := s.handleMessage(ctx, msg)
		if err != nil {
			return Delivery{}, s.fail(err)
		}
		if yielded {
			return delivery, nil
		}
	}
}

// handleMessage dispatches one message the connection received in copy-both
// mode. yielded is true when the message produces a delivery.
func (s *Stream) handleMessage(ctx context.Context, msg pgproto3.BackendMessage) (Delivery, bool, error) {
	switch msg := msg.(type) {
	case *pgproto3.CopyData:
		return s.handleCopyData(ctx, msg.Data)
	case *pgproto3.ErrorResponse:
		pgErr := pgconn.ErrorResponseToPgError(msg)
		return Delivery{}, false, fmt.Errorf("stream from slot %s: %w", s.slotName, pgErr)
	case *pgproto3.CopyDone, *pgproto3.CommandComplete:
		// The server leaves copy-both mode when it shuts down with the
		// slot intact.
		return Delivery{}, false, fmt.Errorf("%w: slot %s at %s", ErrStreamEnded, s.slotName, s.confirmed)
	case *pgproto3.NoticeResponse:
		return s.handleNotice(msg)
	case *pgproto3.ParameterStatus:
		// An asynchronous message the connection has already recorded.
		return Delivery{}, false, nil
	default:
		return Delivery{}, false, fmt.Errorf("%w: ST-4: stream from slot %s received %T outside of copy-both mode",
			ErrInvariantViolation, s.slotName, msg)
	}
}

// warningSeverity is the unlocalized severity of a notice the walsender
// sends when it will not send something.
const warningSeverity = "WARNING"

// handleNotice takes one notice the server sent in copy-both mode. A notice
// below warning is informational. A warning is the walsender saying it will
// not send something — the changes under a publication it skipped loading —
// and is the stream's one chance to stop before a keepalive moves Delivered
// past them.
func (s *Stream) handleNotice(msg *pgproto3.NoticeResponse) (Delivery, bool, error) {
	if msg.SeverityUnlocalized != warningSeverity {
		return Delivery{}, false, nil
	}
	// INV: ST-4 — a change the walsender withheld is never delivered, so a
	// position past it must not be either.
	pgErr := pgconn.ErrorResponseToPgError((*pgproto3.ErrorResponse)(msg))
	return Delivery{}, false, fmt.Errorf("%w: ST-4: stream from slot %s: the server will withhold changes: %w",
		ErrInvariantViolation, s.slotName, pgErr)
}

// waitElapsed reports whether a receive error is only the wait running out:
// the wait's own deadline passed while the caller's context is still live,
// which leaves the connection usable.
func waitElapsed(ctx, waitCtx context.Context, err error) bool {
	return pgconn.Timeout(err) && errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
}

// handleCopyData dispatches one replication message. yielded is true when
// the message produces a delivery.
func (s *Stream) handleCopyData(ctx context.Context, data []byte) (delivery Delivery, yielded bool, err error) {
	if len(data) == 0 {
		return Delivery{}, false, fmt.Errorf("%w: ST-4: stream from slot %s received an empty replication message",
			ErrInvariantViolation, s.slotName)
	}
	switch data[0] {
	case pglogrepl.PrimaryKeepaliveMessageByteID:
		keepalive, err := pglogrepl.ParsePrimaryKeepaliveMessage(data[1:])
		if err != nil {
			return Delivery{}, false, fmt.Errorf("stream from slot %s: keepalive: %w", s.slotName, err)
		}
		return s.handleKeepalive(ctx, keepalive)
	case pglogrepl.XLogDataByteID:
		xld, err := pglogrepl.ParseXLogData(data[1:])
		if err != nil {
			return Delivery{}, false, fmt.Errorf("stream from slot %s: WAL data: %w", s.slotName, err)
		}
		return s.handleWALData(xld)
	default:
		return Delivery{}, false, fmt.Errorf("%w: ST-4: stream from slot %s received replication message %q",
			ErrInvariantViolation, s.slotName, data[0])
	}
}

// handleKeepalive records the server's position, delivers it when it is
// safe to, and answers a reply request. Between transactions the keepalive
// position is one every transaction sent so far committed below, so it is
// delivered; inside a transaction it is not, since the transaction being
// sent commits above it and has not been yielded in full.
func (s *Stream) handleKeepalive(ctx context.Context, keepalive pglogrepl.PrimaryKeepaliveMessage) (Delivery, bool, error) {
	s.serverWALEnd = LSN(keepalive.ServerWALEnd)
	// INV: ST-4 — Delivered never names a position a transaction not yet
	// yielded in full committed at or below.
	if !s.inTransaction {
		s.raiseDelivered(s.serverWALEnd)
	}
	if keepalive.ReplyRequested {
		if err := s.sendStatus(ctx, s.confirmed); err != nil {
			return Delivery{}, false, err
		}
	}
	return s.progress(), true, nil
}

// handleWALData decodes one pgoutput message. The carrying WAL position is
// the change's LSN; the delivered position it arrives with is the stream's
// at that moment, which lies below the transaction's commit. The message's
// server position is not the walsender's: for logical decoding the server
// writes the record's own position into both fields, so only a keepalive
// moves ServerWALEnd.
func (s *Stream) handleWALData(xld pglogrepl.XLogData) (Delivery, bool, error) {
	msg, err := pglogrepl.Parse(xld.WALData)
	if err != nil {
		return Delivery{}, false, fmt.Errorf("%w: stream from slot %s at %s: %w",
			ErrInvariantViolation, s.slotName, LSN(xld.WALStart), err)
	}
	switch msg := msg.(type) {
	case *pglogrepl.BeginMessage:
		if s.inTransaction {
			return Delivery{}, false, fmt.Errorf("%w: ST-4: stream from slot %s at %s: BEGIN inside a transaction",
				ErrInvariantViolation, s.slotName, LSN(xld.WALStart))
		}
		s.inTransaction = true
		return Delivery{}, false, nil
	case *pglogrepl.CommitMessage:
		if !s.inTransaction {
			return Delivery{}, false, fmt.Errorf("%w: ST-4: stream from slot %s at %s: COMMIT outside a transaction",
				ErrInvariantViolation, s.slotName, LSN(xld.WALStart))
		}
		s.inTransaction = false
		s.raiseDelivered(LSN(msg.TransactionEndLSN))
		return s.progress(), true, nil
	case *pglogrepl.RelationMessage:
		return Delivery{}, false, s.recordRelation(msg)
	case *pglogrepl.InsertMessage:
		ev, err := s.decodeInsert(msg, LSN(xld.WALStart))
		return s.deliverChange(ev, err)
	case *pglogrepl.UpdateMessage:
		ev, err := s.decodeUpdate(msg, LSN(xld.WALStart))
		return s.deliverChange(ev, err)
	case *pglogrepl.DeleteMessage:
		ev, err := s.decodeDelete(msg, LSN(xld.WALStart))
		return s.deliverChange(ev, err)
	case *pglogrepl.TruncateMessage:
		return Delivery{}, false, fmt.Errorf("%w: TRUNCATE of %s.%s at %s",
			ErrUnsupportedChange, s.target.Schema(), s.target.Table(), LSN(xld.WALStart))
	case *pglogrepl.TypeMessage, *pglogrepl.OriginMessage:
		return Delivery{}, false, nil
	default:
		return Delivery{}, false, fmt.Errorf("%w: ST-4: stream from slot %s at %s: pgoutput message %T",
			ErrInvariantViolation, s.slotName, LSN(xld.WALStart), msg)
	}
}

// deliverChange stamps a decoded change with the delivered position it
// arrives with and wraps it as a delivery; a decoding error yields nothing.
func (s *Stream) deliverChange(ev *ChangeEvent, err error) (Delivery, bool, error) {
	if err != nil {
		return Delivery{}, false, err
	}
	ev.Delivered = s.delivered
	return Delivery{Change: ev, Delivered: s.delivered}, true, nil
}

// recordRelation checks a relation message against the target and against
// the description the stream already holds; a second description that
// differs is a shape change.
func (s *Stream) recordRelation(msg *pglogrepl.RelationMessage) error {
	rel, err := newRelation(msg, s.target)
	if err != nil {
		return err
	}
	if s.relation != nil && !s.relation.sameShape(rel) {
		return fmt.Errorf("%w: %s.%s", ErrSourceShapeChanged, s.target.Schema(), s.target.Table())
	}
	s.relation = rel
	return nil
}

// changeRelation is the relation a change message refers to, which must be
// the one the stream holds and must arrive inside a transaction.
func (s *Stream) changeRelation(relationID uint32, lsn LSN) (*relation, error) {
	if !s.inTransaction {
		return nil, fmt.Errorf("%w: ST-4: stream from slot %s at %s: change outside a transaction",
			ErrInvariantViolation, s.slotName, lsn)
	}
	if s.relation == nil {
		return nil, fmt.Errorf("%w: ST-4: stream from slot %s at %s: change before any relation",
			ErrInvariantViolation, s.slotName, lsn)
	}
	if relationID != s.relation.id {
		return nil, fmt.Errorf("%w: ST-3: stream from slot %s at %s: change to relation %d, the target is %d",
			ErrInvariantViolation, s.slotName, lsn, relationID, s.relation.id)
	}
	return s.relation, nil
}

func (s *Stream) decodeInsert(msg *pglogrepl.InsertMessage, lsn LSN) (*ChangeEvent, error) {
	rel, err := s.changeRelation(msg.RelationID, lsn)
	if err != nil {
		return nil, err
	}
	columns, err := decodeColumns(rel, msg.Tuple)
	if err != nil {
		return nil, fmt.Errorf("insert at %s: %w", lsn, err)
	}
	key, err := decodeKey(rel, msg.Tuple)
	if err != nil {
		return nil, fmt.Errorf("insert at %s: %w", lsn, err)
	}
	return &ChangeEvent{Kind: Insert, LSN: lsn, Key: key, Columns: columns}, nil
}

// decodeUpdate reads the new image and, when pgoutput sent an old tuple —
// the key under DEFAULT replica identity when it changed, the whole row
// under FULL — the key the row had, which is reported as OldKey only when
// it differs (CO-4: a key-moving UPDATE is a deletion and an image).
func (s *Stream) decodeUpdate(msg *pglogrepl.UpdateMessage, lsn LSN) (*ChangeEvent, error) {
	rel, err := s.changeRelation(msg.RelationID, lsn)
	if err != nil {
		return nil, err
	}
	columns, err := decodeColumns(rel, msg.NewTuple)
	if err != nil {
		return nil, fmt.Errorf("update at %s: %w", lsn, err)
	}
	key, err := decodeKey(rel, msg.NewTuple)
	if err != nil {
		return nil, fmt.Errorf("update at %s: %w", lsn, err)
	}
	ev := &ChangeEvent{Kind: Update, LSN: lsn, Key: key, Columns: columns}
	if msg.OldTuple != nil {
		oldKey, err := decodeKey(rel, msg.OldTuple)
		if err != nil {
			return nil, fmt.Errorf("update at %s: old tuple: %w", lsn, err)
		}
		if oldKey != key {
			ev.OldKey = &oldKey
		}
	}
	return ev, nil
}

// decodeDelete reads the deleted row's key from the old tuple, which both
// admitted replica identities send.
func (s *Stream) decodeDelete(msg *pglogrepl.DeleteMessage, lsn LSN) (*ChangeEvent, error) {
	rel, err := s.changeRelation(msg.RelationID, lsn)
	if err != nil {
		return nil, err
	}
	key, err := decodeKey(rel, msg.OldTuple)
	if err != nil {
		return nil, fmt.Errorf("delete at %s: %w", lsn, err)
	}
	return &ChangeEvent{Kind: Delete, LSN: lsn, Key: key}, nil
}

// raiseDelivered moves the delivered position forward; a position already
// passed is left alone.
func (s *Stream) raiseDelivered(lsn LSN) {
	if lsn > s.delivered {
		s.delivered = lsn
	}
}

// progress is the delivery for a position with no change.
func (s *Stream) progress() Delivery { return Delivery{Delivered: s.delivered} }

// fail records the error that ended the stream and returns it.
func (s *Stream) fail(err error) error {
	s.failed = err
	return err
}

// Close ends the replication connection. The slot persists with the
// position last confirmed.
func (s *Stream) Close(ctx context.Context) error {
	if err := s.conn.Close(ctx); err != nil {
		return fmt.Errorf("close stream from slot %s: %w", s.slotName, err)
	}
	return nil
}
