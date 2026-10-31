# Session affinity: why pg-sprite needs one PostgreSQL session per connection

**pg-sprite requires that every connection it opens stays on one PostgreSQL server session
for as long as the connection is open.** That holds for a direct connection and for a pooler
in session mode. It does not hold behind a transaction-mode or statement-mode pooler, so pg-sprite must
be pointed at a direct or session-mode endpoint. It checks before it executes anything and
refuses an endpoint the check catches, but a check that passes is evidence, not
certification (see [How pg-sprite proves it](#how-pg-sprite-proves-it)). This page explains what depends
on session affinity, how pg-sprite proves it, what it refuses, and how to connect.

## Table of contents

- [Is it mandatory?](#is-it-mandatory)
- [What depends on one session](#what-depends-on-one-session)
  - [Connection, session and backend](#connection-session-and-backend)
  - [What pg-sprite keeps in the session](#what-pg-sprite-keeps-in-the-session)
- [What breaks without it](#what-breaks-without-it)
- [How pg-sprite proves it](#how-pg-sprite-proves-it)
- [Which endpoints pass](#which-endpoints-pass)
- [How to connect](#how-to-connect)
- [What the proof does not cover](#what-the-proof-does-not-cover)
- [Where it is enforced](#where-it-is-enforced)

## Is it mandatory?

Yes, for every command that connects to a database. `migrate` (including `--dry-run` and
`--desired`), `diff`, `pull` and `status` all build their pool through `dbconn.NewPool`, which
proves session affinity before it returns and fails if the proof fails. There is no flag or
option that skips the proof. The commands that never connect (`fmt`, `lint`, `suggest`,
`capabilities`) don't need it.

A library caller is held to the same rule: [`migrate.Run`](../pkg/migrate/migrate.go) and the
other entry points take the pool `dbconn.NewPool` returns, and the table lock session proves
affinity again on its own connection.

## What depends on one session

### Connection, session and backend

PostgreSQL starts one **backend** process for each connection it accepts, and that backend
*is* the **session**: everything set with `SET`, every session-level lock, and every
prepared statement lives in its memory until the connection closes. `pg_backend_pid()` names
it. What varies is how many client connections share a backend, and for how long:

| Path | Client connection → backend | Session state survives across transactions? |
|---|---|---|
| Direct | 1 : 1 for the connection's whole life | Yes |
| Session-mode pooler | 1 : 1 while the client stays connected; the backend is handed to the next client after it disconnects | Yes |
| Transaction-mode pooler | bound for one transaction, then any free backend | No |
| Statement-mode pooler | bound for one statement, then any free backend | No |

```diagram
 direct / session mode                    transaction mode
 ─────────────────────                    ────────────────
 client conn ─────────▶ backend A         client conn ─ tx 1 ─▶ backend A
   tx 1  runs on A                                    ─ tx 2 ─▶ backend B
   tx 2  runs on A                                    ─ tx 3 ─▶ backend A, after
   (A keeps the SETs and locks)                                another client used it
```

### What pg-sprite keeps in the session

So "the connection" pg-sprite holds is only as stable as the backend behind it. Each item
below is state PostgreSQL keeps **per backend**, not per client connection: if the
connection's next statement lands on a different backend, the state is not there.

| Session state | What pg-sprite uses it for | Invariant |
|---|---|---|
| `lock_timeout` and `statement_timeout`, sent as startup parameters and read back by `NewPool` | Bound every lock wait and every statement, so a queued `ALTER` can never sit at the head of the lock queue and block every reader and writer of the table | [LK-2](invariants.md#lk-2--exactly-one-access-exclusive-window-and-every-strong-lock-is-bounded) |
| A session-level advisory lock on a dedicated connection (`pg_try_advisory_lock(0x70677370, hashtext(…))`) | Make sure at most one pg-sprite run works on a table at a time | [LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table) |
| A catalog-first `search_path`, set once per physical connection | Stop a user schema from shadowing `pg_class`, `hashtext` and other catalog names in the engine's unqualified reads | [CO-9](invariants.md#co-9--decision-reads-resolve-to-the-real-catalog) |

`SET LOCAL` work inside a transaction (the role switch and guards in a chunk copy, for
example) needs only the transaction, not the session. The three rows above are why the
session itself has to be stable.

## What breaks without it

Behind a **transaction-mode** pooler, the client connection is bound to a backend only for the
length of one transaction. Between transactions the pooler can hand the next statement to any
backend. Example: one pg-sprite connection whose session state landed on backend A, followed
by an `ALTER` in its next transaction:

```diagram
 client connection                 pooler (transaction mode)        backends
 ─────────────────                 ─────────────────────────        ────────
 session state is set:      ──▶    lands on backend A        ──▶    A: lock_timeout=3s,
   timeouts, search_path                                               catalog-first path
 SELECT pg_try_advisory_lock ──▶   runs on backend A         ──▶    A: holds the table lock
   → true
 (idle between transactions)       A goes back to the pool;
                                   another client now runs on
                                   A and "holds" pg-sprite's lock
 ALTER TABLE ... (next tx)   ──▶   runs on backend B         ──▶    B: lock_timeout=0
                                                                     (server default: wait
                                                                      forever)
```

(pg-sprite sends its timeouts as startup parameters. A transaction-mode PgBouncer rejects
startup parameters it doesn't track, or is configured to drop them; `NewPool` reads the
values back and refuses a connection that doesn't carry them, with
`ErrSessionSettingsDiscarded`. The proof below covers the case where the settings did land,
but only on one backend.)

Three things went wrong, and none is visible to the client:

1. **The `ALTER` runs unbounded.** Backend B never saw `lock_timeout`, so if the table is busy
   the `ALTER` waits at the head of the lock queue indefinitely, and every query on the table
   queues behind it. That is the outage the timeouts exist to prevent.
2. **The table lock no longer excludes anyone.** The advisory lock lives on backend A, which
   now serves another client. A second pg-sprite run whose statement lands on A sees the lock
   as its own, and two runs can work on the same table at once.
3. **Catalog reads may resolve differently.** Backend B may carry a `search_path` that lets a
   user schema shadow catalog names.

Behind a **statement-mode** pooler it is worse: every statement can land on a different
backend, and the pooler refuses multi-statement transactions outright, which pg-sprite needs
for every chunk copy, guarded write, checkpoint save and cutover.

## How pg-sprite proves it

Reading back `lock_timeout` is not enough on its own: an idle transaction-mode pooler may hand
the same backend back twice and report the settings healthily. So after the read-back,
`NewPool` tries to force the rebind a pooler would eventually make, and checks for it:

```diagram
 conn  (one of the pool's connections)      second connection (throwaway pool of 1)
 ────                                       ─────────────────────────────────────
 t1  pg_try_advisory_lock(random key) → true
 t2                                         SELECT 1 (takes a backend first)
                                            BEGIN   (pins it for the whole tx)
 t3                                         pg_try_advisory_lock(same key)
                                              healthy: false (conn's session holds it)
 t4  does pg_locks show my own backend
     as the holder of the key?
       healthy: yes
 t5                                         ROLLBACK
 t6  pg_advisory_unlock(key) → true
       healthy: true (conn is still the session that took it)
```

- **Direct connection or session-mode pooler:** `conn` is one session throughout, so every
  reading is the healthy one, and there are no false positives to trade off.
- **Transaction-mode pooler:** the proof tries to expose the rebind. The open transaction at
  t2 is meant to take the backend `conn` used at t1, so that `conn`'s next statement lands
  elsewhere; when it does, at least one reading fails: the second connection takes a lock
  `conn` "holds", `conn` is no longer the holder in `pg_locks`, or `conn` can't release what it
  took. `NewPool` then returns `ErrNoSessionAffinity` with the remedy. A pooler with spare
  backends can hand the second connection a different backend, and then every reading is
  the healthy one: **a passing proof does not make a transaction-mode endpoint safe.** Point
  pg-sprite at a direct or session-mode endpoint whether or not the proof passes.
- **Statement-mode pooler:** the second connection has just run `SELECT 1`, so a pooler that
  refuses the `BEGIN` at t2 keeps no backend across statements. `NewPool` returns
  `ErrNoSessionAffinity` before any work runs.
- The key is freshly random on every call, so concurrent proofs never contend and a probe lock
  stranded on a pooled backend can't block anything later.
- The proof needs **one spare connection** to the server, or pooler, beyond the pool's own.

The same proof runs on the table lock's dedicated connection before the lock is attempted
(`dbconn.AcquireTableLock`), so the lock is never taken on a connection that can't keep it.

## Which endpoints pass

| Endpoint | Result |
|---|---|
| Direct connection to PostgreSQL (including Aurora/RDS writer endpoints) | Passes |
| PgBouncer, or another pooler, in **session** mode | Passes. The proof needs one spare server connection in the pooler's pool. |
| PgBouncer in **transaction** mode | Not supported. Usually refused with `ErrNoSessionAffinity`, or with `ErrSessionSettingsDiscarded` when the pooler rejects or drops the timeout startup parameters. A proof that passes does not make it supported. |
| PgBouncer in **statement** mode | Not supported. Refused with `ErrNoSessionAffinity` (PgBouncer rejects the proof's `BEGIN`), or with `ErrSessionSettingsDiscarded` as above. |
| Supabase Supavisor session endpoint | Passes ([supabase.md](supabase.md)) |
| Supabase Supavisor transaction endpoint | Not supported. Refused with `ErrNoSessionAffinity` in testing, even with named prepared statements disabled ([supabase.md](supabase.md)) |

The session-, transaction- and statement-mode PgBouncer results are tested against a real
PgBouncer ([testing.md](testing.md)). Both refusals name the same remedy: connect through a
direct or session-mode endpoint.

## How to connect

- **Point pg-sprite at a direct endpoint or a session-mode endpoint.** Applications can keep
  using a transaction-mode pooler; pg-sprite is one more client of the same database, not a
  replacement for the application's connection path.
- **On hosted PostgreSQL** the session endpoint is usually the same host on a different port.
  The refusal's remedy says so.
- **Size the pooler for pg-sprite:** in session mode each pg-sprite connection holds a server
  connection for its whole life. A run needs its pool, the dedicated table lock session, the
  proof's spare connection, and (for copy-and-swap on a live table) replication connections,
  which never go through a transaction-mode pooler.
- **Keep idle limits longer than the table lock keepalive.** The lock session confirms it still
  holds the lock every 10 seconds by default. A pooler or server idle timeout
  (`idle_session_timeout`, a pooler's own idle limit) shorter than that can end the session,
  and pg-sprite then aborts the run (LK-1, fail closed).

## What the proof does not cover

- **It is evidence, not certainty.** An idle transaction-mode pooler with spare backends can in
  principle answer every reading healthily. The proof is a guard against the configuration an
  operator lands on by following a hosted platform's default connection string, not a
  substitute for pointing the engine at the right endpoint.
- **It runs once per pool, on one connection.** Connections the pool opens later are not
  re-proven; they inherit the endpoint the first one proved.
- **Dedicated sessions from `dbconn.ConnectSession`** are not proven. They are used for work
  that is not tied to one session the way a session-level lock is: a transaction-scoped
  advisory lock on a slot name, and the identity check of a copy-and-swap connection.
- **Replication connections** are not proven. They are opened from the same connection
  settings and must reach the same database; a transaction-mode pooler can't carry them.

## Where it is enforced

- [`pkg/dbconn/dbconn.go`](../pkg/dbconn/dbconn.go): `NewPool` reads the timeouts back
  (`verifySessionSettings`) and runs the proof (`proveSessionAffinity`).
- [`pkg/dbconn/affinity.go`](../pkg/dbconn/affinity.go): `ProveSessionAffinity`, the proof.
- [`pkg/dbconn/sessionstate.go`](../pkg/dbconn/sessionstate.go): `ErrNoSessionAffinity`,
  `ErrSessionSettingsDiscarded`, and the remedy text.
- [`pkg/dbconn/table_lock.go`](../pkg/dbconn/table_lock.go): `AcquireTableLock` proves
  affinity on the lock session before attempting the lock.
- [invariants.md, LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table) and
  [LK-2](invariants.md#lk-2--exactly-one-access-exclusive-window-and-every-strong-lock-is-bounded).
