package dbconn

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// affinityProbeFloor is the least time the proof's own statements are given.
// They are a handful of trivial round trips, so anything near this has
// already failed.
const affinityProbeFloor = 15 * time.Second

// affinityProbeBound is how long the whole proof gets on a pool whose dials
// are budgeted at connectTimeout. The second connection the proof needs is
// opened lazily inside this bound, so its dial spends the dial budget before
// the proof's first statement runs. The two are added rather than maxed: an
// operator who budgeted a long dial because the server genuinely takes that
// long to reach would otherwise watch the proof spend its whole budget
// dialing and report a deadline instead of the affinity verdict it exists to
// give.
func affinityProbeBound(connectTimeout time.Duration) time.Duration {
	if connectTimeout <= 0 {
		return affinityProbeFloor
	}
	return affinityProbeFloor + connectTimeout
}

// ProveSessionAffinity proves on conn the property every session-scoped
// advisory lock rests on: that this connection keeps one server session, so
// a lock taken on it is a lock other connections cannot take. It returns an
// error wrapping ErrNoSessionAffinity when it proves the property does not
// hold, so a caller fails closed instead of running without the exclusion it
// believes it has. pinner supplies a second connection to the same server
// and is not otherwise disturbed.
//
// The proof takes a lock on conn and then makes a second connection hold a
// transaction open across the check. A transaction-mode pooler pins a
// backend for a transaction's duration, so the second connection takes the
// backend conn's single-statement acquire just released, and conn's next
// statement lands somewhere else. That turns a rebind that would otherwise
// depend on load into one the proof can observe, and it gives three
// independent readings of the same failure: the second connection takes a
// lock conn holds, conn no longer appears in pg_locks as the session holding
// its own lock, and conn cannot release what it took.
//
// Against a direct connection, and against a pooler that hands out a session
// per client connection, all three readings are the healthy one. There is no
// false positive to trade off, because each reading is a fact about the
// connection in hand rather than a guess about what sits behind it.
//
// bound caps the whole proof; a non-positive bound leaves it on the
// caller's context alone.
//
// It is one-sided in the other direction: an idle transaction-mode pooler
// with spare backends can answer every reading the healthy way, so a clean
// proof is evidence and not certainty. It is a guard against the
// configuration an operator lands on by following a hosted platform's
// default connection string, not a substitute for pointing the engine at a
// direct endpoint.
//
// The probe key is freshly random on every call, so concurrent proofs never
// contend and a probe lock stranded on an unreachable backend can never
// block anything later.
//
// INV: LK-2 — every strong lock acquisition is bounded. The bounds are
// session settings, so they only hold where the session does. An advisory
// lock is the instrument here rather than the subject: it is the one piece
// of session state whose loss a client can observe directly, which makes it
// the way to prove the session is stable enough to carry the timeouts.
func ProveSessionAffinity(ctx context.Context, conn *pgxpool.Conn, pinner *pgxpool.Pool, bound time.Duration) error {
	if bound > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}

	key, err := probeKey()
	if err != nil {
		return err
	}
	var taken bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&taken); err != nil {
		return fmt.Errorf("take the session affinity probe lock: %w", err)
	}
	if !taken {
		// Nothing else can hold a freshly random key, so a refusal means the
		// acquisition did not land where it reported.
		return fmt.Errorf("%w: a freshly keyed probe lock was reported as already held", ErrNoSessionAffinity)
	}

	reentered, stillHeld, err := probeAcrossPinnedBackend(ctx, conn, pinner, key)
	if err != nil {
		return err
	}
	if reentered {
		return fmt.Errorf("%w: a second connection took an advisory lock this one holds", ErrNoSessionAffinity)
	}
	if !stillHeld {
		return fmt.Errorf("%w: the connection that took an advisory lock is no longer the session holding it", ErrNoSessionAffinity)
	}

	var released bool
	if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil {
		return fmt.Errorf("release the session affinity probe lock: %w", err)
	}
	if !released {
		return fmt.Errorf("%w: the connection that took an advisory lock could not release it", ErrNoSessionAffinity)
	}
	return nil
}

// probeAcrossPinnedBackend holds a transaction open on a second connection
// and, from inside it, reports whether that connection can take the advisory
// lock conn already holds, and whether conn still appears as the session
// holding it.
//
// The open transaction is the point: it denies conn the backend it acquired
// on, so conn's read runs wherever the connection is routed next.
func probeAcrossPinnedBackend(ctx context.Context, conn *pgxpool.Conn, pinner *pgxpool.Pool, key int64) (reentered, stillHeld bool, err error) {
	tx, err := pinner.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("begin the session affinity probe transaction: %w", err)
	}
	// The probe writes nothing, so the rollback is the whole cleanup: it
	// ends the transaction, unpins the backend, and drops any lock the probe
	// took on it.
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && err == nil {
			err = fmt.Errorf("roll back the session affinity probe transaction: %w", rollbackErr)
		}
	}()

	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&reentered); err != nil {
		return false, false, fmt.Errorf("probe the advisory lock from a second connection: %w", err)
	}
	if reentered {
		// Undo the re-entry so the lock count on the shared backend goes
		// back to what conn took, leaving conn's own release meaningful.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			return false, false, fmt.Errorf("undo the probe advisory lock re-entry: %w", err)
		}
	}
	stillHeld, err = sessionHoldsAdvisoryLock(ctx, conn, key)
	if err != nil {
		return false, false, err
	}
	return reentered, stillHeld, nil
}

// AdvisoryLockHolder is the query surface a lock confirmation needs;
// *pgxpool.Conn and *pgx.Conn both satisfy it. The confirmation must run on
// the session under test, so this is deliberately not the pool.
type AdvisoryLockHolder interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// sessionHoldsAdvisoryLock reports whether the session behind conn is the
// one holding the advisory lock for key.
//
// pg_locks reports a single-argument advisory key as objsubid 1 with the
// key's high and low halves in classid and objid, both unsigned OIDs, so the
// halves are split client-side and compared as bigints — a key whose high
// half looks negative as an int32 must still match.
func sessionHoldsAdvisoryLock(ctx context.Context, conn AdvisoryLockHolder, key int64) (bool, error) {
	classID, objID := advisoryLockCatalogKey(key)
	const query = `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_locks
		 WHERE locktype = 'advisory'
		   AND granted
		   AND objsubid = 1
		   AND classid::bigint = $1
		   AND objid::bigint = $2
		   AND pid = pg_backend_pid()
	)`
	var held bool
	if err := conn.QueryRow(ctx, query, classID, objID).Scan(&held); err != nil {
		return false, fmt.Errorf("read the advisory lock holder from pg_locks: %w", err)
	}
	return held, nil
}

// advisoryLockCatalogKey splits an advisory lock key into the (classid,
// objid) pair pg_locks reports for it: the high and low 32 bits, each
// widened back out of the unsigned OID the catalog stores.
func advisoryLockCatalogKey(key int64) (classID, objID int64) {
	return int64(uint32(uint64(key) >> 32)), int64(uint32(uint64(key)))
}

// probeKey returns an advisory lock key no other caller can be using.
func probeKey() (int64, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, fmt.Errorf("generate a session affinity probe key: %w", err)
	}
	return int64(binary.BigEndian.Uint64(nonce[:])), nil
}
