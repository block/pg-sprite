# Copy-and-swap: v1 design decisions

This page records the decided v1 shape of the copy-and-swap executor. The lifecycle narrative
remains in [low-level-design.md](low-level-design.md#copy-and-swap-executor-lifecycle), and runtime
rules remain in [invariants.md](invariants.md); when either disagrees with this page on a v1
detail, this page is authoritative.

## v1 scope

The reason strings below are the planned typed-refusal vocabulary. The
[capability matrix](capabilities.md) continues to describe shipped behavior and will be updated
when the executor ships.

| Classification | v1 surface |
| --- | --- |
| Supported in v1 | Rewrite-requiring `ALTER COLUMN … TYPE`, initially `integer` → `bigint` identity primary keys, `text` → `varchar(n)`, and `numeric` precision/scale widening; volatile-default `ADD COLUMN`; and `STORED` generated-column addition. The table is permanent, has one `smallint`, `integer`, or `bigint` primary-key column (a usable primary key), and has a replica identity of `DEFAULT` or `FULL`; no incoming or outgoing foreign keys, triggers, partitioning/inheritance, or rules; no dependent views or materialized views; no publication other than the engine's own publishing it (explicit membership, `FOR ALL TABLES`, or `FOR TABLES IN SCHEMA`); no subscription applying into it; no other object depending on its OID or row type; and no `FORCE ROW LEVEL SECURITY`. Every name the route derives is a fixed width under PostgreSQL's 63-byte identifier limit (`NAMEDATALEN - 1`), so no source name can be refused for length. Foreign keys, triggers, dependent views, publications, subscriptions, and the remaining `pg_depend` dependents are what a rename swap strands (RF-2): they would follow the retained `_old` table, not the live one, or publish the shadow's copy writes. |
| Typed refusal (planned) | Unsupported key (`copy-and-swap-pk-unsupported`); unsuitable replica identity (`copy-and-swap-replica-identity`); foreign keys (`copy-and-swap-foreign-keys`); triggers or rules (`copy-and-swap-triggers`); partitioned or inherited tables (`copy-and-swap-partitioned`); unlogged tables (`copy-and-swap-unlogged`); forced row-level security (`copy-and-swap-force-rls`); dependent views or materialized views (`copy-and-swap-dependent-views`); a publication other than the engine's own (`copy-and-swap-publication-member`); a subscription applying into the table (`copy-and-swap-subscription-target`); any other OID- or row-type-bound dependent (`copy-and-swap-dependents`); unavailable logical decoding (`copy-and-swap-logical-decoding-unavailable`); insufficient replication-slot or WAL-sender capacity (`copy-and-swap-slot-headroom`); a same-named slot owned by another database or held as a physical slot (`copy-and-swap-slot-collision`); or insufficient disk (`copy-and-swap-disk-headroom`). Insufficient grants are refused by the tiered privilege check that runs first (`*preflight.PrivilegeError`, reason `insufficient-privileges`), naming the exact `GRANT`. Every refusal names its reason. |
| Out of scope | All other table shapes and operations, including primary-key changes, receive a typed refusal rather than an unsafe approximation. |

## Decisions

### D1 — No durable scratch database

**Decision.** Under `SET LOCAL ROLE <owner>`, the executor creates the shadow in the source schema with
`CREATE TABLE <shadow> (LIKE <source> INCLUDING ALL EXCLUDING IDENTITY)`, then executes the gated
user `ALTER TABLE` against that empty shadow. The server therefore applies and validates the DDL
without readers or a table-sized rewrite. The after-schema fingerprint used for checkpoint
compatibility comes from the existing transaction-scoped `pkg/schemadiff` scratch schema. This is
execute-and-introspect, not AST transformation. No durable scratch database or `CREATEDB` grant is
required.

The gated statement names the source table, so reaching the shadow needs exactly one edit at the
parse boundary: `pkg/statement` retargets the statement's relation to the shadow's name and
reprints it through the PostgreSQL deparser — the same single-field-and-deparse shape the
safer-sequence rewrite uses to add `CONCURRENTLY`. No other node changes; the semantics of the
DDL remain the server's. The executor re-verifies the retargeted statement before running it:
re-parsed, it must equal the gated statement in every respect except the target relation, which
must equal the shadow (the ST-7 check, with the shadow as the permitted target).

**Which columns are copied.** The copy list names a source column only when it is the same
column on the shadow after the statement, not merely a column of the same name. A column the
statement dropped and added again under one name is a new column: it takes its own default (or
NULL) on the shadow, as it would under a direct `ALTER TABLE`, and the dropped column's values are
not copied into it. The shadow is cloned with its columns numbered 1..n in the source's live column
order, a retype keeps a column's attribute number, and an added column is numbered past n, so a
source column at live position `i` is the same column exactly when the shadow's column of that
name has attribute number `i`. The build and a resume's inspection both read the numbers from the
catalog, so they derive the same list, and the copier, the checksum digest, and the live flusher
all take their columns from it (ST-11).

**Comparison-only exception.** `RowSecurityChange.CanonicalSQLForNamespace` may erase
only the validated RLS operation target's schema on a fresh parsed tree. This supports
comparison across physical schemas when the key separately carries the canonical
namespace and table. It preserves policy names, roles, expressions, qualified helpers,
statement order, and duplicates. Its output is never executable SQL and never replaces
the executor's exact comparison of reviewed SQL under lock. Original statements and the
fully qualified canonical representation remain unchanged.
Validate the expected physical schema and table from the caller’s target configuration;
never derive those expected values from the SQL being checked. This exception does not apply
to shadow DDL generation or checkpoint fingerprints.

**Why.** The real server remains the semantic authority while the only durable temporary object is
the shadow needed by the operation itself.

**Alternative considered → deferred.** A separately provisioned durable scratch database adds
privilege and lifecycle requirements without strengthening the proof. Executing the statement
verbatim against a same-named shadow in an engine-owned schema reached through `search_path`
avoids the retarget but cannot handle a schema-qualified statement and would move the swap from
a rename to a `SET SCHEMA`; it was rejected for the two behaviours it would fork.

**Where enforced.** `pkg/schemachange` shadow builder and `pkg/schemadiff`; ST-2, ST-6, ST-7 (the
retarget re-verification, with the shadow as the sole permitted target), ST-11 (the copy list).

### D2 — Build indexes and constraints up front

**Decision.** `INCLUDING ALL` carries column defaults, constraints, indexes, per-column storage
settings (`SET STORAGE`), and the comments on columns, constraints, and indexes when the shadow
is created; identity is the D5 exception. It does not carry the table's owner, ACLs,
row-level-security setting and policies, storage parameters (`reloptions` such as
`fillfactor` and autovacuum settings), the table comment, or the replica identity
(`pg_class.relreplident`); the shadow builder replicates those explicitly — for replica
identity, `ALTER TABLE … REPLICA IDENTITY` on the shadow, so a source on `FULL` swaps in on
`FULL` (D6) — and the ST-5 fidelity checklist refuses to swap until they match. `INCLUDING ALL`
also copies a `CHECK … NOT VALID` constraint as a **validated** one, and the copier's insert
would then reject rows the source legally holds; for every source constraint with
`pg_constraint.convalidated = false` the shadow builder drops the copied constraint and re-adds
it `NOT VALID` on the still-empty shadow, so the shadow carries the source's actual semantics
and the swap never silently validates a constraint the user left unvalidated (ST-5 compares
`convalidated`). Bulk copy pays index maintenance as rows arrive.

**Why.** The shadow has its complete enforced shape throughout copy and reaches the fidelity gate
without a second long build phase.

**Alternative considered → deferred.** Building secondary indexes after copy may be faster, but
needs separate resumability, validity, uniqueness, and resource controls.

**Where enforced.** `pkg/schemachange` shadow builder and `pkg/preflight`; ST-5, ST-6.

### D3 — Store checkpoints in the target database

**Decision.** The engine role creates `pgsprite_checkpoint` in the target database on first use,
in an engine-owned `pgsprite` schema — the target's schemas are never written to, and `public`
is not assumed writable (PostgreSQL 15 revoked `CREATE` on it from `PUBLIC`). It contains one
row per `(schema, table)`, keyed on the row format version and the two model fingerprints
(ST-2); a write for a target whose row carries another statement's fingerprints or another
format is refused, typed, and a fresh start deletes the row explicitly first. A non-terminal
row carries the proof of the shadow the run built (`shadow_proof`), since after the swap the
catalog can no longer re-mint it from the shadow's name, and the text of the statement the
run was given (`statement`), written once with the row and never replaced, so a resume can
refuse a statement other than the one the shadow embodies before it writes anything; a resume
reads the row's proof, statement, and phase (`Store.Inspect`, guarded on the row format and the phase alone)
before it knows which statement it is resuming. The table's shape is derived from its columns, not recorded beside them: an
engine that finds a table an older engine created adds the columns it lacks, additively and
under the same advisory key as the create, and refuses — with no DDL — a table carrying
columns it does not know, which a newer engine shaped (ST-10); the proof, once a row holds
one, is write-once — a different shadow is a fresh start, not an overwrite. Creating the schema needs `CREATE` on
the database, the same privilege the scratch schema already needs.

**Why.** Ordinary table data follows the database through failover and gives each target one
atomic, local resume record.

**Alternative considered → deferred.** An external or append-only store introduces another
consistency boundary and unbounded history.

**Where enforced.** `pkg/checkpoint`; ST-1, ST-2, ST-10.

### D4 — Restrict the chunk key to one integer-family primary key

**Decision.** v1 accepts one non-deferrable `smallint`, `integer`, or `bigint` primary-key
column. It uses
monotonic PK-range chunks and may discard captured changes above the copier watermark under
CO-4, judged **per key**: an UPDATE that moved the primary key (`ChangeEvent.OldKey` set) is a
deletion of the old key and an image of the new one, each judged against the watermark on its
own, so the deletion below the watermark is applied even when the new key above it is discarded.
Composite, `uuid`, text, and `DEFERRABLE` keys return `copy-and-swap-pk-unsupported`.

**Why.** A totally ordered, compact key makes chunk boundaries, resume watermarks, and the
watermark-discard proof share one representation. A deferrable key is checked at the end of
each statement rather than per row, so one statement can move a row onto a key another row still
holds; the decoded stream then no longer has one row per key, which the CO-5 buffer's refusals
and its carry-forward of the old key's image both rest on, and the server declines to use such a
key as the DEFAULT replica identity, so every UPDATE on the published table would fail.

**Alternative considered → deferred.** Queue-mode apply for arbitrary keys remains a later
extension.

**Where enforced.** `pkg/preflight`, `pkg/copier`, and `pkg/applier`; CO-4, ST-6.

### D5 — Hand off sequences and identity explicitly

**Decision.** For `serial` and explicit `DEFAULT nextval(...)` columns, `INCLUDING DEFAULTS`
causes source and shadow to share the source sequence, which keeps its name; cutover runs
`ALTER SEQUENCE … OWNED BY <new-table>.<column>` so the sequence survives the D9 drop as the live
table's. Identity metadata is excluded from the shadow. The shadow instead receives
`DEFAULT nextval('<source-identity-sequence>')`. While writers are excluded by
`ACCESS EXCLUSIVE`, cutover renames the source identity sequence to its `_old` name (D8), drops
that default, recreates identity with
`ALTER TABLE … ALTER COLUMN … ADD GENERATED ALWAYS|BY DEFAULT AS IDENTITY` using the source
sequence's declared options — except that a bound the source left at its own integer type's
limit follows the column to the new type's limit when the gated statement widened the column
(`integer` → `bigint` moves a default `MAXVALUE 2147483647` to `9223372036854775807`; a bound
the user set explicitly inside the old range is kept), since replaying the old limit would
have the new sequence stop where the old type stopped — renames the server-named new identity
sequence to the name the source's held (freed by its `_old` rename, so a user-renamed identity
sequence keeps its name), re-grants on it every `USAGE`, `SELECT`, and `UPDATE` privilege the
source's sequence carried (the server creates the new sequence with the owner's default ACL,
and a role that called `nextval` through the source's grant would otherwise lose it at the
swap), and copies the source sequence's exact position with
`setval('<new-identity-sequence>', last_value, is_called)`, both values read in the same
transaction from the sequence relation itself (`SELECT last_value, is_called FROM <sequence>`)
— not from `pg_sequences.last_value` or `pg_sequence_last_value()`, which report NULL for a
never-advanced sequence and would turn the strict `setval` into a silent no-op. A never-advanced
source (`is_called = false`) therefore leaves the new sequence at the same not-yet-issued start
value rather than skipping it, and no sequence value is ever spliced into SQL text. Discovery
uses `pg_get_serial_sequence` and verifies the corresponding `pg_depend` ownership edge. The old
identity sequence is dropped with the old table. A change that drops an identity column removes
it from the handoff: the shadow has no column to carry the default, so the proof lists only the
identity columns the shadow kept, and that sequence too ends with the old table.

The shared counter puts a dependency edge in the direction an operator does not expect: the
shadow's default depends on the *source's* sequence. `DROP TABLE <source>` therefore fails
while a shadow exists, and the hint the server offers — `DROP TABLE <source> CASCADE` — drops
the shadow's identity default silently, leaving a shadow whose key column has no default at all.
Cleaning up after an aborted run drops the shadow **first**, never the source with `CASCADE`
(`DropShadow` drops exactly the shadow, without `CASCADE`, and `InspectShadow` refuses a shadow
whose identity default no longer points at the source's sequence); the same ordering governs
the D9 drop of the retained `_old` table, which by then owns nothing the live table depends on.

**Why.** Copy never advances an unrelated sequence, and the post-swap table preserves generation
kind, sequence name, and the source's exact issued-value state.

**Alternative considered → deferred.** Copying identity through `INCLUDING IDENTITY` creates an
independent sequence too early and obscures the state handoff.

**Where enforced.** `pkg/schemachange` shadow builder and cutover; ST-5.

### D6 — Preserve omitted TOAST values

**Decision.** v1 requires a table with a usable primary key whose replica identity is `DEFAULT`
or `FULL`, and never changes the user's replica identity: the shadow is set to the source's
(D2) and ST-5 refuses to swap if they differ. UPDATE apply is column-wise: only fields that
carry a value in the pgoutput new tuple are assigned, so an unchanged TOASTed field remains
untouched.

**Why.** Regardless of replica identity, pgoutput sends an unchanged out-of-line value in the new
tuple as an unchanged-TOAST marker (column type byte `u`) rather than its bytes: the column is
present in the tuple — the column count is always the full count — but carries no value.
`FULL` enlarges only the old tuple. Treating the marker as a value would corrupt the shadow.

**Alternative considered → rejected.** Requiring or setting `REPLICA IDENTITY FULL` does not
remove the marker; it only increases WAL and mutates user configuration.

**Where enforced.** `pkg/decode` and `pkg/applier`; CO-8, ST-6.

### D7 — Checksum through the destination types

**Decision.** Each chunk computes `sha256(string_agg(row_hash, ''::bytea ORDER BY pk))` on both sides,
where `row_hash` is `sha256(convert_to(row::text, getdatabaseencoding()))`; only the chunk's hash is rendered
as hex. Every compared source value is cast to the shadow column's type before its row hash is
formed; a column the statement retyped with `USING` is compared as that expression, cast to
the shadow column's type, so the source side hashes the value the copy put in the shadow rather
than what an explicit cast of the source column would make of it. SHA-256 rather than `md5`:
PostgreSQL built against OpenSSL routes `md5()` through it, and an OpenSSL in FIPS mode
refuses MD5, which would fail every pass on such a host; the digest
never leaves the process, so the hash has no compatibility surface.
The digest compares the copy list only: a generated column is absent from both the copy and the
comparison, since the shadow recomputes it on insert, and so are a dropped column and a column
the statement dropped and added again under the same name ([D1](#d1--no-durable-scratch-database)).

**Why.** One comparison shape proves the stored post-change representation for widening,
narrowing, and precision changes instead of comparing unlike textual representations.

**Alternative considered → deferred.** Operation-specific checksum SQL multiplies semantic edge
cases and test surfaces.

**Where enforced.** `pkg/checksum` — the `Verifier`'s digest statement casts every copy column
to the type the shadow declares on both sides, and on the source side carries the shadow
proof's `Conversions` in the converted columns' place (the generated-column half is planned);
CO-1, CO-2.

### D8 — Use deterministic bounded names

**Decision.** Shadow and retained-source names are `_pgsprite_<16-hex-hash-of-schema.table>_new`
and `_pgsprite_<16-hex-hash-of-schema.table>_old`. `LIKE` derives the shadow's index names — and
so the names of the primary-key, unique, and exclusion constraints those indexes back — from the
shadow's name, so cutover restores them: every index, extended-statistics object, and identity
sequence on the old table is renamed to `_pgsprite_<hash>_old_<16-hex-hash-of-dependent-name>`, and the shadow's
corresponding dependent is then renamed to the name the source dependent held
(`ALTER INDEX … RENAME` renames a constraint together with its index; `CHECK` constraints keep
their names under `LIKE` and need no rename). `LIKE` keeps none of the source's index,
unique-constraint, or statistics names, so "corresponding" is established by **definition**, not
by name: indexed columns or expressions, access method, operator classes, uniqueness, and
predicate for an index; the column set and kinds for a statistics object. One difference is set
aside: PostgreSQL re-creates an index on a column the gated statement retypes for the new type,
re-deriving that type's default operator class and the column's own collation while keeping an
operator class or collation written in the index, so for a key column the statement retyped —
the same name on both sides, a different canonical type — the default operator class and the
column's own collation do not count, and everything else about the two definitions, a written
operator class or collation included, must still agree; an expression or a predicate over such
a column (the server may render either differently for the new type), and every column the
statement did not retype, is held to an exact match. Two source indexes with identical
definitions are interchangeable, so an arbitrary pairing between them restores an equivalent
catalog; two that are identical only once the set-aside facts are removed are not shown to be,
so the relaxed rule pairs neither and both stay unpaired. The post-swap catalog therefore
carries the user's names — `ON CONFLICT ON CONSTRAINT u_slot` keeps working and a later change
of the same table derives the same names — and only the old table's dependents wear the suffix.
A shared `serial`/`nextval` sequence is not a dependent of the old table and keeps its name (D5).
Every derived name is a fixed width — 30 bytes for the table names, 47 for a dependent's —
because both the table and the dependent enter the name as hashes, never as text; no source
name, however long, can push a derived name into the server's silent truncation at 63 bytes, so
the route has no name-length refusal. The hash has no inverse, but a derived relation always
lives in its source's schema, so `schemachange.SourceOfDerivedName` recovers the source by
recomputing each table's derived names in that one schema — the query an operator runs on
finding a `_pgsprite_…` relation they did not create. The lookup names every catalog relation,
function, and operator by schema, so its answer is the same on a caller-built pool whatever that
pool's `search_path` (CO-9).

**Why.** Stable names make catalog inspection and resume deterministic; restoring user names keeps
the swap invisible to code that names constraints; fixed-width hashed names make truncation, and
the collisions it would cause, unreachable.

**Alternative considered → deferred.** Random names simplify initial creation but make recovery
depend on extra durable mapping state.

**Where enforced.** `pkg/preflight` and `pkg/schemachange`; ST-2, ST-6.

### D9 — Drop the old table after commit by default

**Decision.** Cutover renames the source to the deterministic `_old` name. After commit, the
executor drops it in a separate bounded statement. `--keep-old` skips that drop.

**Why.** The default returns disk promptly without extending the atomic swap transaction. An
operator who needs an opt-in rollback window can retain the table.

**Alternative considered → deferred.** Retention by default doubles storage for an unbounded
period.

**Where enforced.** `pkg/schemachange`; LK-2, LK-4, ST-5.

### D10 — Cut over as soon as the gate passes

**Decision.** v1 has no `--defer-cutover`; successful fidelity and checksum gates proceed to a
bounded cutover attempt.

**Why.** A waiting state lengthens slot and disk exposure and adds control states without helping
the core online path.

**Alternative considered → deferred.** Operator-triggered deferred cutover remains a possible
later mode.

**Where enforced.** `pkg/schemachange`; CO-1, LK-2.

### D11 — Bound and reap logical-decoding state

**Decision.** Slot and single-table publication are both named `pgsprite_<8hex>`, where the hash
covers `database.schema.table`: a replication slot is cluster-wide while the checkpoint table
(D3) is per-database, so two databases holding a same-named table must not derive the same slot
name. If a slot of the derived name already exists for another database, or as a physical slot — a hash
collision or a foreign naming choice — preflight refuses (`copy-and-swap-slot-collision`) rather
than sharing or dropping it; a logical slot of the derived name in the current database is the
route's own earlier slot and is left to the reaper or the resume path. Preflight also sets the
engine's publication aside from the publication refusal by this exact derived name, so a
publication that merely wears the `pgsprite_` prefix is treated as somebody else's. The
slot is created and the checkpoint row that names it committed under the engine's advisory
lock on the slot's name (`checkpoint.Store.WithSlotLock`, transaction-scoped on a connection
dialled outside the pool and proven to be on the pool's cluster and database, so the claim's own work on the pool never waits on the seat the lock
sits in), which the reaper holds from its last read of the row to its drop, so a slot with no
row is an orphan and never a slot mid-claim. Preflight checks for a free slot under
`max_replication_slots` and two free senders under `max_wal_senders`, since the slot's connection
holds its exported snapshot while the copy imports it and the stream decodes on a second one.
Slot lag has a hard byte ceiling, default 1 GiB; crossing it aborts
fail closed. At startup a reaper drops orphan `pgsprite_*` slots that belong to the current
database (`pg_replication_slots.database = current_database()`), are inactive
(`active_pid IS NULL`), and whose checkpoint row is absent or terminal; slots of other databases
are never inspected, let alone dropped. An operator who needs to reclaim a slot by hand runs
the same pass through `checkpoint.Store.ReapOrphanSlots`; failing that, PostgreSQL's own
`SELECT pg_drop_replication_slot('<name>')` on the slot's database drops the slot (the
publication of the same name is dropped separately with `DROP PUBLICATION`), after confirming
no checkpoint row in a non-terminal phase still names it. Slot inspection treats
`pg_replication_slots.wal_status = 'lost'` as lost state.

**Why.** A slot is durable cluster state that can retain unbounded WAL unless ownership and a
hard limit are explicit — and because it is cluster state, ownership must be established per
database, or a reaper in one database would push another's in-flight change into ST-4 slot loss.

**Alternative considered → deferred.** Best-effort cleanup alone cannot cover process death.

**Where enforced.** `pkg/decode` (`CreateSlot` creates the publication and then the slot under the derived name, on a replication connection proven to be on the pool's own cluster and database; `DropSlot` drops only a logical slot of the pool's own database, over a connection proven the same way, and only a publication of the shape `CreateSlot` makes, treating any other slot or publication of the name as foreign; `InspectSlot` reads `wal_status` and retained WAL, unknown rather than zero on a server in recovery, with the server's own account of a lost slot — `invalidation_reason`, `conflicting`, `inactive_since` — where the server version carries it; `SlotStatus.LostState` is the one reading of a status as lost and the `SlotLostError` it returns names the server's cause or says none was reported; `SlotStatus.WithinLagCeiling` judges a slot against the ceiling — lost before any measure, unknown never read as nothing retained, over it a typed `SlotLagExceededError`), `pkg/checkpoint` (`Store.ReapOrphanSlots` is the reaper: it lists the logical, engine-prefixed slots of `current_database()` and then reads the non-terminal checkpoint rows that name them, and re-reads each slot's row right before dropping it, under the engine's lock on the slot's name, which is the read that decides; a slot no live row names is dropped through `DropIdleSlot`, which refuses a held slot rather than wait for its holder, so whether a slot is held is judged at the drop; a slot a live row names, a slot a process holds, and a name wearing the prefix without the engine's shape are reported and left, and a database without the checkpoint table is refused rather than read as all orphans), `pkg/applier` (`Catchup` ends every cycle by inspecting the stream's slot and judging it against `CatchupOptions.SlotLagCeiling`, default `decode.DefaultSlotLagCeiling`, which cannot be set below one byte; a vanished slot is `SlotLostError`), `pkg/schemachange` (`InspectResume` reads a checkpoint row against the catalog and answers clean or reconcile; `Reconciler.Reconcile` is the ST-4 reconcile mode — row to `reconciling`, drop whatever bears the derived name, fresh slot, repair pass, row back to the phase the watermark says, the row and the slot under `WithSlotLock`; `Runner.Run` on a decoding target creates the slot and the row that names it under `WithSlotLock`, leaves a stopped run's slot under its row for the next `Run` to reopen the stream from — a lag-ceiling abort included — and drops the slot once the swap has committed), and `pkg/preflight` (`CopySwapDecodingName` derives the name; `CheckCopySwapEnvironment` refuses the collision); ST-3, ST-4, ST-6.

### D12 — Throttle by chunk time and slot lag

**Decision.** The copier adjusts chunk size toward a 500 ms target and also obeys D11's hard slot
lag ceiling.

**Why.** Time-targeted chunks adapt to row width and server capacity while keeping feedback and
checkpoint intervals bounded.

**Alternative considered → deferred.** Replica-lag-based throttling needs topology-specific
observation and policy.

**Where enforced.** `pkg/copier` (`Chunker`: chunks are sized in rows and cut by keyset from the
live table, so sparse and dense key spaces yield equal work per chunk; each timing feedback scales
the measured chunk's own row count toward the target by at most a factor of two, within a configured
floor and ceiling, so concurrent workers' reports do not compound; `Copier` times each chunk from claim to commit on an injected clock and feeds it back), `pkg/decode` (`WithinLagCeiling`), and `pkg/applier` (`Catchup` judges the slot every cycle); LK-3, ST-3.

### D13 — Recover unique-secondary-key moves batch-wide

**Decision.** A buffered flush applies the CO-5 buffer — one merged image or deletion per primary
key, with no statement order to preserve — in one transaction as key-targeted upserts and
column-wise updates (D6) plus deletes. Flush boundaries fall on source commit boundaries, so a
batch never holds half of a source transaction. On SQLSTATE `23505` the applier rolls back to the
flush's savepoint and reapplies the whole batch as delete-all-then-insert-all: it deletes every
key in the batch, then inserts every surviving image. CO-6's test obligation stays open until
this converges under test; its fixed vector is `seats(id int PRIMARY KEY, slot text UNIQUE)`
holding `(1,'A'),(2,'B')` with the batch `{1→'B', 2→'A'}`.

The fallback inserts whole rows, so it needs complete images where the primary path needs only
present columns. Two rules supply them. First, the CO-5 buffer merges rather than replaces: a
newer UPDATE image overlays only its present columns onto the buffered image for that key, so an
unchanged-TOAST marker (`u`, D6) survives dedup only when no buffered image for the key ever
carried that column's value — that is, the value lives in a shadow row the batch did not
create: the row for the key itself, or, when the UPDATE moved the primary key
(`ChangeEvent.OldKey` set), the row for the old key, since a key-moving UPDATE emits a
marker-bearing image under a key the shadow has never held — and when the row moved more than
once inside the window, the old key is the one the row **started at**, the only key whose shadow
row can hold its pre-buffer version (CO-5). Second, before the fallback deletes
anything it completes every surviving image that still carries a marker by reading those columns
from the current shadow row — the row for `Key`, or for `OldKey` when the image carries one
(`SELECT … FOR UPDATE` on the affected keys, in the same transaction, after the savepoint
rollback; the old key's row is still present because completion runs before any delete); by
CO-8 the shadow's stored value is exactly the value the marker stands for. That read presumes
the shadow row is present, which holds only under one of the two chunk-overlap disciplines CO-4
admits: a key inside an in-flight chunk may have its flush **deferred** until the chunk lands, or may be
flushed now with a tombstone retained and re-applied afterwards
([low-level-design](low-level-design.md#copy-and-apply-ordering-the-core-correctness-subtlety)).
Under tombstone retention a marker-bearing UPDATE for such a key would flush while the copier
has not yet written the row, and the fallback would find no shadow row on a permitted
interleaving. D13 therefore fixes the applier's choice: a flush that touches any key inside an
in-flight chunk is deferred until that chunk lands (chunk copy and backlog flush mutually
excluded per overlapping key range); the tombstone-retention form is not available to the v1
applier. With that discipline, the row a marker-bearing image of a key that did not move stands
for pre-exists on the source under that key, so the key is either above the cut frontier
(discarded under CO-4 at drain) or inside an in-flight chunk (whose flush is deferred) — and the
shadow holds its row, unless the source deleted the row before the chunk covering the key read
it: that chunk never copied it, and the DELETE has not reached the stream yet. Absence is
therefore not by itself a protocol error. Before it writes, the flush checks that the shadow
holds a row for every such image, in one statement that also reads the server's
`pg_current_wal_insert_lsn()`. For an absent row it writes nothing and invents nothing: it
returns the image in `Result.Held` with that position and `Entry.AbsenceAwaited` set, and the
buffer takes it back (`Buffer.Hold`) and lets it in once the stream has delivered every change
committed at or below that position (`Buffer.Release`). The deletion that explains the absence
committed before the chunk's read, which came before the check, so by then the stream has
delivered it: a DELETE has replaced the image with a delete marker, and a later INSERT with a
whole image, each applied as any other entry. An image that comes back with the flag still set
— even if UPDATEs delivered meanwhile have filled in every marker — is one the stream still holds
live under a key whose shadow row is absent, which no ordering of source changes produces: the
applier aborts the change fail closed (CO-8). A moved image is different: its old key's shadow row
holds the row's pre-buffer version only if the chunk covering the old key was read **before** the
move. A move captured while the old key was uncut, or inside a chunk read after the move, leaves
no shadow row under either key, and the buffer cannot tell the two histories apart. And when the
source puts **another row** at the old key before the copier reads it — an INSERT, a different
row's move onto it, or an UPDATE of a key the buffer does not hold — the chunk the copier reads
copies that row, so the old key's shadow row is present but is not this row's; the buffer sees the
reuse when the event arrives, marks every buffered image that left the key (`Entry.OldKeyReused`,
inherited through a chain of moves; a row returning to the key it started at is not a reuse),
and a flagged image is never completed from the old key's shadow row. The flush therefore reads
a moved image's completion from the old key's shadow row when that row is present and the image
is not flagged, and otherwise from the **source** row under `Key`, in the flush's transaction.
Neither read is the value the marker stood for by construction: the shadow row under the old
key was copied live and the source row is live, so either can already hold a change the stream
has not delivered — most sharply, another row's value, when a later source transaction put a
different row at the key. The flush does not write a completion it read this way. It returns
the image with its completion and the server's `pg_current_wal_insert_lsn()` at the read, taken
in the same statement, as a `HeldImage`; the buffer takes it back (`Buffer.Hold`) with the
completion pending, and `Drain` keeps the image until the stream has delivered every change
committed at or below that position (`Buffer.Release`). A change that could have been what the
read saw arrives before then as an event for the key — an UPDATE keeps the completion pending
and overlays its own values, so a value the stream delivered is never replaced by a read; any
event that replaces the key's entry, or reuses the image's old key, drops the completion, since
the read may have seen that event's row. A completion that survives to its release was read
from a row no event has touched since, so the stream knows the same row under the key and the
value is the marker's own; the next drain flushes the whole image. An image neither row can
complete is a key the source no longer holds — a later event deletes or moves it again — and the
flush writes a delete marker at `Key` rather than invent a value or leave a row the batch did
not land. The buffer keeps the image and the old key's entry together across the drain (CO-4)
so the old key's row is neither deleted nor copied between the read and the apply, and the
drained `Batch` names the images to complete in `CompleteFirst`, as copies of its `Entries`:
`Entries` are in key order, which puts a moved image after or before the delete marker at its
old key as the keys happen to sort, so the flush reads every completion `CompleteFirst` names
before it writes anything, rather than reading the batch in slice order. The column-wise pass
writes every delete marker before any image, so a row that moved to a lower key while keeping a
unique value finds its old row gone before its upsert; a batch that moves a unique value
between rows still collides, and the fallback catches SQLSTATE `23P01` (an exclusion constraint)
as it does `23505`, since an exclusion over a moved value fails the same way. CO-6's second test
vector therefore moves the primary key of a row whose out-of-line column is untouched, and
asserts the image is held with the old key's value and flushes complete once released; a third
vector moves a row whose old key never landed and asserts the hold carries the source's value;
a fourth puts another row at the old key after the move and asserts the held image never takes
that row's value.

**Why.** PostgreSQL's targeted `ON CONFLICT` cannot by itself move one unique secondary value
between rows, and neither can per-key retry: for the cyclic exchange above, a delete-then-insert
pair collides in both orders, so a per-pair loop reaches no fixed point. Deleting every key in the
batch first leaves no row that can collide with the inserts, so the batch-wide form converges in
one pass — provided every inserted image is complete, which the two rules above guarantee
without inventing a value for an omitted column.

**Alternative considered → rejected.** Per-key delete-then-insert retry cannot converge on a
cyclic exchange. Ignoring secondary conflicts would leave convergence to the checksum rather
than the applier. Restricting the fallback to batches whose images are already complete would
leave a cyclic exchange that touches a TOASTed row with no converging path at all.

**Where enforced.** `pkg/applier`; CO-5, CO-6.

### D14 — Make divergence policy explicit

**Decision.** The CLI accepts `--on-divergence=abort|repair` and defaults to `abort`; the parser a
setting goes through (`ParseDivergencePolicy`) refuses `recopy`. After slot loss, resume
internally selects `recopy` — the repair without its reread — for one pass at or below the
watermark, and the next pass under the caller's policy is the proof.

**Why.** Ordinary divergence indicates a correctness defect; slot-loss reconciliation is the one
mode where known missing changes require repair. The mode must not be inferred from wiring.

**Alternative considered → deferred.** A universal repair default can conceal executor defects.

**Where enforced.** `pkg/checksum` `Check` takes a `DivergencePolicy` on every pass and refuses
the zero value (`ErrNoDivergencePolicy`); `abort` returns a `DivergenceError` with the shadow
untouched, `repair` recopies every differing chunk with the copier's own statement in one
transaction and reads each again, `recopy` is the same recopy with no reread, and a pass that
recopied anything under either mints no proof. `pkg/schemachange` selects `recopy` in exactly one
place: `Reconciler.Reconcile`, the ST-4 reconcile mode, whose one pass runs at or below the
checkpointed watermark after a fresh slot is in place, so the only changes it can be recopying
are the ones the lost slot missed — and the source is live while it runs, so a reread would
refuse a chunk a write landed on after the recopy that the new slot already carries; every other
pass takes the caller's policy; CO-2, CO-3, ST-4. In the catch-up stage the caller's policy runs
through `CheckLive`, which settles every differing chunk against the catch-up's confirmed
position before it judges the chunk divergent, and which refuses `recopy`, so no live pass
ever recopies without reading again; it is the shape of Spirit's lockless checker (issue #1233,
the checksum no longer taking the table lock and instead rereading a differing chunk once the
replication client has applied past it), carried over as an idea, not as code. The compare and the recopy are separate
statements against a live source, so a source that moves a unique or primary-key value between
rows in the window between them can make the recopy's insert collide with a shadow row the
compare judged equal; the one recopy transaction rolls back, the pass ends in error with the
row still in `reconciling`, and the next attempt restarts the reconcile — a retry that survives
the move is a tracked follow-up.

### D15 — Capture changes with pgoutput

**Decision.** `pkg/decode` uses pinned `jackc/pglogrepl` with PostgreSQL's built-in `pgoutput`
plugin. Trigger capture remains the documented alternative in
[change-capture-tradeoff.md](change-capture-tradeoff.md), but is not built in v1.

**Why.** Logical decoding keeps synchronous work out of application writes, while pglogrepl owns
the load-bearing streaming-replication and pgoutput wire expertise.

**Alternative considered → deferred.** Trigger capture supports clusters without logical
decoding but adds write-path availability and amplification costs.

**Where enforced.** `pkg/decode` and `pkg/preflight`; ST-3, ST-4, ST-6.

## Live verification

`Verifier.CheckLive` is the checksum pass for the catch-up stage: the source is written while
the pass runs and the stream, through `applier.Catchup`, is the shadow's only other writer. The
sweep is `Check`'s — every chunk cut and digested on both sides in one `REPEATABLE READ`
snapshot — but a chunk that differs in the sweep is not yet a finding, because the shadow may
simply not have received the source's latest writes. The pass settles each differing chunk
before it decides anything.

**Marking a read.** In the chunk's own transaction, after both digests, the pass writes a
marker to the WAL: `pg_logical_emit_message(false, 'pgsprite', '')`, a non-transactional
logical message with an empty payload, and keeps the position the server returns for it.
Every transaction the snapshot saw committed before the marker, so once the catch-up has
confirmed past the marker's position (`ConfirmedSource.Confirmed`, which `applier.Catchup`
satisfies, as it does `FlushGate`, the hold a live repair takes on its flushes), every source
change the read saw is in the shadow. The marker is used rather than
`pg_current_wal_insert_lsn()` because the walsender decodes only flushed WAL, and the insert
position can sit in a tail that nothing flushes while another session holds a transaction
open after its last write; a catch-up cannot confirm what the walsender cannot read, and the
pass would report a caught-up stream as lagging. The message is written at once and the
marking transaction's commit asks the WAL writer to flush through it, so the stream reads past
it whatever else is open. The message names no table and the stream does not ask for
messages, so the decoder skips it and nothing is written to the shadow.

**Settling.** The pass waits for the catch-up to confirm past the marker — polled every
`LiveOptions.Poll`, for as many polls as fit in `LiveOptions.SettleWait` — reads the chunk
again in a fresh snapshot, and judges the two reads together:

| The reread finds | Verdict | Counts as |
| --- | --- | --- |
| both sides equal | settled | clean (`Report.Settled`) |
| the shadow holding exactly what the sweep saw on the source, the source having moved on | settled | clean — the shadow caught up to a known source state and the newer change is still in the stream |
| the source or the shadow moved and the sides still differ | hot | another reread, up to `LiveOptions.Attempts` in all, then `*UnresolvedError` (cause `hot`) |
| neither side moved across a confirmed position and the sides still differ | diverged | a confirmed divergence (`Report.Mismatches`) |
| the catch-up did not confirm past the marker within the wait | — | `*UnresolvedError` (cause `lagging`) |

Every differing chunk is settled, so one chunk that will not settle does not hide a divergence
in another; the `Report` counts the unsettled chunks (`Report.Unsettled`) beside the settled
ones and lists the confirmed divergences.

**Outcomes.** A pass whose every chunk is equal or settled mints the proofs as `Check` does,
and the `VerifiedShadow` records that a live pass minted it (`VerifiedShadow.Live`): the shadow
equals the source as of the positions the pass settled against, and every change since is
still in the stream. A pass with unsettled chunks and no divergence returns the first as an
`*UnresolvedError`: not a finding about the data, the shadow as the pass found it, for the
caller to run the pass again once the catch-up has confirmed further or the source is quieter.
Under `abort` a confirmed divergence is a `*DivergenceError` with the shadow untouched, whether
or not other chunks were unsettled. Under `repair` every confirmed divergence is recopied in
one guarded transaction with the catch-up's flushes held off the shadow for as long as it
runs: the pass first takes the hold in Go (`LiveOptions.Flushes`, a `FlushGate`;
`applier.Catchup.Pause` returns once no flush is in flight and lets none start until its
resume), so no flush interleaves with the delete and the copy, no flush waits behind the
recopy's lock and runs out its `lock_timeout` however long the recopy takes, and the
catch-up keeps reading and confirming the stream under the hold — answering the
walsender's keepalives, buffering what the source writes, and confirming no further than
the oldest buffered change (ST-4) — so the first cycle after the resume flushes what the
hold buffered. The recopy transaction still takes `SHARE MODE` on the shadow, as the
guarantee against every other writer: a flush the gate does not hold, or a writer that is
not the stream's, waits until the recopy commits (the flushes that then land replay images
the recopy already holds, or changes committed after it); the pass marks the WAL after the
copy and before the commit, resumes the flushes once the recopy has committed, waits for the
catch-up to confirm past the marker, and settles each repaired chunk the same way — a
confirmed divergence after its repair is a `*RepairError` (CO-2), and a pass whose repairs
all took but which left other chunks unsettled returns the first of them as an
`*UnresolvedError` with the repairs committed and listed. A pass with repairs mints no proof.
`recopy` is refused for a live pass, since it reads nothing again and a live pass settles what
it recopies (CO-3). The hold lasts for the whole recopy, so a divergence that spans many
chunks buffers the stream's changes for as long as the copy takes, one buffer entry per
changed key, holding its full column values; no flush's `lock_timeout` ends the hold, and the
slot retains the WAL below the oldest buffered change until the resume lets it flush. Every
cycle under the hold still checks the slot, so a hold whose retained WAL outgrows the slot's
WAL ceiling ends the catch-up with `SlotLagExceededError` (ST-3, fail closed) and the pass
then reports the chunk `lagging`: under heavy writes that ceiling is the bound that fires
first. `applier.Status.FlushesHeld` reports the hold while it lasts, so a caller watching
`Lag` grow and `Applied` stand still can tell a repair holding the flushes from a stalled
catch-up.

**At the swap.** `GateCutover` accepts a live proof like a quiesced one. `Cutover` refuses a
live proof handed to it with no `DrainFunc` (`cutover-undrained`, CO-1): the proof covers the
shadow only up to the positions the pass settled against, and the drain is what applies the
rest of the stream under the swap's `ACCESS EXCLUSIVE` lock, after which no row can change
before the rename. A caller that stops the stream and re-verifies with a quiesced `Check`
holds a proof that needs no drain. The rule throughout stands on the stream being the shadow's
only other writer; a row whose key moved across a chunk boundary between the two reads is the
next pass's to catch.

## Package and proof-type map

| Package | Responsibility and proof types | Invariants |
| --- | --- | --- |
| `pkg/dbconn` | Produces `TableLock`, carried by `TableLockSession`; `Confirm` is the in-transaction check every writer runs from its own connection before its first write. | LK-1 |
| `pkg/preflight` | Produces `CopySwapShape` (the table facts `PreflightedTable` carries plus the v1 shape, replica identity, and dependent-object checks above, minted by `CheckCopySwapShape`) and from it `CopySwapTarget`, the copy-and-swap route's proof, minted only by `CheckCopySwapEnvironment` once the decoding and headroom facts are proven against that shape; `CheckCopySwap` runs the privilege, shape, and environment checks as one call; owns Tier-3 refusals. | ST-6, RF-1..RF-3 |
| `pkg/copier` | Produces `Chunk` and `Watermark`; `Chunker` (built only from a `CopySwapTarget`) cuts consecutive chunks that tile the whole int64 key space — first open below, last open above — so every key a row can carry belongs to exactly one chunk and a watermark at the largest value means the copy is complete. `Copier` (built from a `CopySwapTarget`, a `Shadow` — the shape `schemachange.BuiltShadow` satisfies — and the table's `TableLockSession`) copies chunks with several workers, each in its own bounded transaction under the owner's role that takes `ACCESS SHARE` on the source and `ROW EXCLUSIVE` on the shadow, confirms the lock, and confirms both relation OIDs before one frozen never-overwriting insert — the chunk statement `pkg/internal/chunksql` holds for the copier and the verifier's repair alike, and so does its `search_path`: the catalog alone for a copy without conversions, and for a copy with them the shadow build's own path (catalog first, then the source schema, then `public`), so a USING expression resolves its unqualified names in the copy as it did when the statement ran on the shadow, and the rendering settings a text conversion depends on (`chunksql.Rendering`: `extra_float_digits = 3`, `bytea_output = 'hex'`), pinned in every chunk transaction so a float or `bytea` converted to `text` lands as the flush and the digest render it; a resumed copy first deletes every shadow row above the watermark in batches of the same guarded shape — the whole shadow after a zero watermark — so the resumed cut frontier and the shadow agree on what is uncut, and its first batch's guard takes the shadow in `SHARE ROW EXCLUSIVE` as its one lock on it (two fences queue rather than deadlock), so a chunk transaction of the earlier run still committing into it, or paused before its insert after confirming the lock, ends before the clear reads; the proof check refuses a shadow that is the source by name or by OID, since the clear deletes from one and the copy reads the other; the copy selects each column the `Shadow` proof names in `Conversions` as that column's USING expression — read from the statement by `statement.ColumnConversions`, since the catalog keeps no record of a USING once `ALTER COLUMN TYPE` has run — and every other column bare, so a retyped column without USING takes the server's assignment cast, and the proof check refuses a conversion of a column the copy does not carry, with no expression, or of the primary key, whose values every chunk range, the watermark, the resume clear, and the conflict target are expressed in; `Position` snapshots the cut frontier and landed watermark as two `Watermark` values plus the in-flight chunks, and `Position.Classify` is the applier's uncut / in-flight / landed rule. While `Run` is in progress the `Copier` is the `progress.Tracker`'s `WorkSource` (`Options.Tracker`): `rows_copied` is the rows this run's committed chunks inserted, `rows_total` the source's catalog row count read once at the start of the run, and `bytes_copied` / `bytes_total` the shadow's and the source's `pg_table_size` measured at each poll — every counter measured, none projected; on a resumed run the row ratio is that run's share, not the copy's completion. Each measurement, and each chunk cut, runs in a read-only transaction of its own under the chunk budgets with the catalog alone on its `search_path`, so a poll or a cut on a caller-built pool is still bounded (LK-2) and still resolves to the real catalog (CO-9); the copier stops being the source before `Run` returns. Every refusal the copier returns is a `*copier.RefusalError` carrying a `copier.RefusalCause` from the closed set in [refusal-classes.md](refusal-classes.md#copier-refusals-keyed-on-copierrefusalcause), fail-closed under `ErrInvariantViolation`; a chunk whose insert collides with a shadow row on a unique index or exclusion constraint (SQLSTATE `23505` / `23P01`) — a value the catch-up has moved between rows and the flush has yet to rewrite — is run again after a bounded linear wait (`Options.CollisionAttempts`, `Options.CollisionBackoff`; `Collisions()` counts them) and, when every attempt collides, the server's `*pgconn.PgError` is returned wrapped; any other chunk the shadow itself rejects is that error as it stands, and neither carries a cause. | CO-4, CO-9, LK-1, LK-2, LK-3 |
| `pkg/checksum` | `Verifier` (built from a `CopySwapTarget`, a `copier.Shadow`, and the table's `TableLockSession`) compares the source with its shadow up to the copier's landed watermark and returns a `Report` of the chunks that differ. It cuts its own chunks with a `copier.Chunker`, cutting and digesting each chunk in one read-only `REPEATABLE READ` transaction, so the cut and the chunk's two digests describe one snapshot, no snapshot outlives one chunk, and the cut's lock wait is bounded like the reads (LK-2); each transaction runs the copier's guard — owner role, the copy statement's `search_path` (the catalog alone, or the shadow build's path when the proof carries conversions, so a repair's recopy resolves a USING expression as the copy did), `ACCESS SHARE` on both relations taken before the snapshot, lock confirmation, relation-OID check — and pins the rendering settings the copy, the flush, and the digest share (`chunksql.Rendering`: `extra_float_digits = 3`, `bytea_output = 'hex'`), since the digest hashes each row's text rendering and a database or role configured otherwise would render two floats that differ only in their last digits the same, or a `bytea` converted to `text` in the escape form the copy never wrote. Both sides run the same frozen statement — row count plus hex `sha256` of the key-ordered concatenation of each row's `sha256(convert_to(ROW(col::shadow_type, …)::text, 'UTF8'))` over `pk BETWEEN $1 AND $2` (SHA-256 so a FIPS-mode OpenSSL, which refuses `md5()`, does not fail the pass) — so only the data can differ, and the cast on every column is D7: a retyped column hashes as the value the shadow holds, and a column the statement retyped with `USING` hashes on the source side as that expression cast to the shadow's type, the one place the two statements differ. Only the copy columns are compared; generated columns present on both sides are not yet hashed. A `Report` proves nothing. `Check` runs the same pass under a `DivergencePolicy` the caller states every time (the zero value is refused): `abort` returns a `DivergenceError` carrying the report with the shadow untouched; `repair` replaces every differing chunk inside one guarded read-write transaction — delete the shadow's rows over every chunk's key range, then the copy statement itself for every chunk, so a unique value the source moved from one differing chunk to another lands instead of colliding with the stale row — and digests each chunk again in a fresh snapshot, returning a `RepairError` for the first that still differs alongside the `Outcome` listing every repair that committed. A repair pass assumes nothing else writes the shadow and the source rows it recopies hold still until the rereads; a write inside the pass reads as a `RepairError`, never as a second repair; `recopy` is the same recopy without the rereads, for a source that is written while the pass runs, and reports what it recopied the same way. `ParseDivergencePolicy` lets a caller refuse a configured policy before any pass; it accepts `abort` and `repair` and refuses `recopy`, which no setting may name. `CheckLive` is the pass for the catch-up stage, where the source is written and the stream is the shadow's only other writer; it is described under [Live verification](#live-verification). Only a pass that found nothing and repaired nothing mints the proofs, whose constructors are private to the package: a `CleanWatermark` at the watermark it read through, and a `VerifiedShadow` only when that watermark is complete; `Outcome.Clean` is true only when the clean watermark was minted. A pass with repairs returns its `Repair`s and no proof; the next pass mints. While `Verify` or `Check` runs — through the repair phase as well — the verifier is the tracker's `progress.WorkSource` (`Options.Tracker`): `chunks_compared` and `rows_hashed` count the comparison's two-sided digests that have committed and the source rows they covered, `chunks_mismatched` the chunks the comparison found differing, `chunks_repaired` the chunks whose one recopy transaction has committed, and `chunks_reread` the chunks digested again after the comparison — the repaired chunks under `Check`, every settling reread under `CheckLive` — every counter read from the pass's memory, none from the database, and all reset when a pass starts; a pass started while another runs on the same verifier is refused. | CO-1, CO-2, CO-3, CO-9, LK-1, LK-2 |
| `pkg/decode` | Creates, drops, and inspects the route's slot and publication under the derived name (`CreateSlot` with exported snapshot and consistent point; `DropSlot` waiting out a holder, idempotent, never crossing clusters or databases, dropping only the route's own publication; `InspectSlot` with `wal_status` and retained WAL, the latter unknown rather than zero on a server in recovery; `SlotStatus.WithinLagCeiling` judging a slot against a ceiling in bytes — a `lost` slot is `SlotLostError` before any measure, an unknown measure is `ErrSlotLagUnknown` and never within the ceiling, over it is `SlotLagExceededError` carrying both figures; `DefaultSlotLagCeiling` is one GiB; `IsEngineSlotName` tells the engine's name shape from a name merely wearing its prefix; `InspectSlot` also reads the server's account of a lost slot — `invalidation_reason`, `conflicting`, `inactive_since` — where the server version carries it, one statement across every supported major, and `SlotStatus.LostState` renders it into the `SlotLostError` so an operator reads *why* from the error). `OpenStream` decodes the slot with pgoutput from a position — the consistent point, or the applied position a resume checkpointed — on a dedicated replication connection proven to be a session of the pool's cluster on the target's database — the same table on another cluster derives the same slot name — into one `ChangeEvent` per committed row change: every column carried with its presence, so the unchanged-TOAST marker is an absent `Column` and NULL a present one — built by `PresentColumn` and `AbsentColumn`, read through `Get`, which returns the value together with its presence, so a reader takes the presence with the value — and `OldKey` for an UPDATE that moved the primary key under either admitted replica identity. A relation other than the target, a tuple that does not line up with it, or a change outside a transaction fails closed; a table whose shape changed under the stream is `ErrSourceShapeChanged`, a TRUNCATE is `ErrUnsupportedChange`, and a warning the server sends in copy-both mode — the walsender saying it will withhold changes, as from a publication it skipped loading — stops the stream fail-closed before a keepalive can deliver a position past them; the replication connection asks for warnings itself (`SET client_min_messages = warning` on the open session, the last word on the setting), so no role, database, or server setting, login event trigger, URL parameter, or connection pooler can keep them from it. The stream keeps `Delivered` — moved by a commit or by a keepalive between transactions, never past a transaction not yet yielded in full — apart from `Confirmed`, the one position it reports to the server: `Confirm` refuses a regression or a position beyond `Delivered`, and a keepalive reply carries only what the caller confirmed, so the slot's resume point moves on the caller's word alone. A wait that runs out in `Next` sends the same report, so the stream is never silent for longer than the caller's wait (`Next`'s doc says why that matters to the walsender). Both positions order transactions by their commit, so a change's own LSN can lie below either; each `ChangeEvent` carries the `Delivered` it arrived with, which is what a caller may confirm while the change is unapplied, and the server replays by commit position, so a resume never skips a replayed change for lying below its checkpoint. `ServerWALEnd` is the walsender's send position from its last keepalive and never below `Delivered`; a change does not move it, since its message carries only the change's own position. `ServerWALEnd − Delivered` is a lower bound on lag — the walsender reports how far it has decoded, not how far the server has written — so lag against the server's WAL end is measured on the pool with `pg_current_wal_lsn()`. The server ending replication with the slot intact is `ErrStreamEnded`, the clean restart ST-4 keeps apart from slot loss. | ST-3, ST-4, CO-4, CO-8 |
| `pkg/applier` | `Buffer` merges events into one `Entry` per primary key and `Drain` judges every key against the copier's `Position`, returning a `Batch` whose `CompleteFirst` names the moved, marker-bearing images the flush must complete before it writes. `Flusher` (built from a `CopySwapTarget`, a `copier.Shadow`, and the table's `TableLockSession`) applies one batch in one guarded read-write transaction — owner role, the copy statement's `search_path` (the catalog alone, or the shadow build's path when the proof carries conversions), `ACCESS SHARE` on both relations, lock confirmation, relation-OID check, the copier's guard — bound to the lock session's context so a lost lock cancels the flush in flight. A column the statement retyped with `USING` is written through that expression (D7): the flush reads the column's source type from the catalog inside its transaction and binds the decoded value as `(SELECT <expr> FROM (SELECT $n::<source type> AS "col") AS "source")`, so the value lands as the copy would have landed it, while a completion read from the shadow is already in the shadow's form and is bound bare; the flusher refuses, as ST-6, a proof whose conversions name the primary key, a column the copy does not carry, or an empty expression, and a converted column the source no longer carries. It first reads a completion for every `CompleteFirst` image: from the old key's shadow row, read `FOR UPDATE` so the batch's own delete of that key cannot race the read; from the source row under `Key` when `OldKeyReused` or when the old key's shadow row is absent (a move captured while the old key was uncut left no shadow row). Neither row is known to be the stream's, so the image is not written: it returns in `Result.Held` as a `HeldImage` with its completion and the `pg_current_wal_insert_lsn()` the read took in the same statement, and `Buffer.Hold` takes it back with the completion pending — an event that replaces the key's entry or reuses its old key drops the completion, an UPDATE keeps it pending behind its own values, and `Buffer.Release(passed)` fills the markers still left once the stream has delivered every change committed through the read's position, so the next `Drain` flushes the whole image (D13). An image neither row can complete is written as a delete marker at `Key`, since the source holds no row there, and its key named in `Result.Skipped`. A marker-bearing image of a row that did not move whose shadow row is absent — the source deleted the row before its chunk was read and the DELETE is still on its way — is held the same way, with no completion and `Entry.AbsenceAwaited` set, and flushed once the stream has passed the position of the check that found the row absent; an image that comes back flagged for a row still absent is a CO-8 `ErrInvariantViolation` (D13). The primary path then applies the batch column-wise inside a savepoint, every delete marker before any image: a delete marker is a `DELETE`, a whole image an `INSERT … ON CONFLICT (pk) DO UPDATE`, and a marker-bearing image of a row that did not move an `UPDATE` of only its present columns — zero rows updated is a CO-8 `ErrInvariantViolation`, never an insert that invents the omitted value. On SQLSTATE `23505` or `23P01` (a unique index or an exclusion constraint) the flush rolls back to the savepoint and reapplies the batch whole-row (D13): every remaining marker is completed from the shadow row under its own key `FOR UPDATE` (absent is a CO-8 refusal), every key in the batch is deleted in one statement, and every surviving image is inserted plain; a second collision is `ErrBatchDeferred`, and `Buffer.Requeue` puts the batch back — refusing with a CO-5 `ErrInvariantViolation` if any of its keys is held, since nothing may `Add` between `Drain` and the flush's outcome — so the caller drains again once the copier has landed the chunk the rows wait on. `Result` counts the images and deletes written and the completions, carries the held images and the skipped keys, and says whether the fallback ran. The flush pins the same rendering settings as the copy and the digest (`chunksql.Rendering`: `extra_float_digits = 3`, `bytea_output = 'hex'`) so a float or a `bytea` the decoder rendered lands, through a `USING` conversion to `text`, as the text the copy wrote and the verifier will hash, and the completion reads carry the same pin so a held value lands as the shadow renders it. `Catchup` owns the loop: it gathers the stream into the buffer, `Release`s held images at the delivered position, drains against the copier's `Position` read *after* the last `Add` (so a key the copier cuts between the read and the drain is judged in flight, never discarded), flushes, and on `ErrBatchDeferred` requeues the batch for the next pass while a chunk is in flight — once the copy has landed every key, a refusal is one no chunk can resolve, and `Run` ends with `ErrBatchDeferred` rather than requeue it forever. It confirms the lesser of the delivered position and `Buffer.OldestPending` — each keyed on the `Delivered` the event arrived with, never the event's own LSN — so the slot never advances past a change that is still buffered, held, or deferred, and never falls below what the stream had already confirmed (ST-4); the confirmation follows the requeue or hold, so it never reads a buffer emptier than the one the next drain sees. The whole loop runs under the lock session's `Bind` context, so a lost lock ends a quiet wait on the stream as it ends a flush, reported as `ErrInvariantViolation` wrapping `dbconn.ErrTableLockLost`. `Run` never returns nil: it runs once and returns the context's cause when its owner stops it, carrying any cycle error that is not the stop itself alongside. It is the `progress.WorkSource` of a catch-up step: `changes_applied`, `changes_buffered`, and `lag_bytes` measured against `pg_current_wal_lsn()` read on the pool as each cycle ends, never against the walsender's send position. Each cycle then inspects the stream's slot on the pool and judges it against `CatchupOptions.SlotLagCeiling` (zero takes `decode.DefaultSlotLagCeiling`; below one byte is refused, so the ceiling is always in force): a slot that vanished or that the server reports `lost` ends `Run` with `decode.SlotLostError`, and one retaining more WAL than the ceiling ends it with `decode.SlotLagExceededError` — the table lock stays held and the slot stays in place, so the operator reads what the slot retained and the resume path decides what follows (D11, ST-3). | CO-4, CO-5, CO-6, CO-8, LK-1, LK-2, LK-3, ST-3 |
| `pkg/checkpoint` | Produces `Checkpoint` and persists it: `Store` (over a `pkg/dbconn` pool) creates `pgsprite.pgsprite_checkpoint` on first use (`Ensure`, serialized under the engine's advisory key so concurrent first users never race the create; it creates only what is absent, so a pre-provisioned schema and table the engine owns need no database `CREATE`, refuses with `ErrForeignObject` a schema or table another role owns, brings a table an older engine created up to this engine's shape by adding the columns it lacks — the shape is derived from the table's columns, and every column added after the first shape is nullable so rows survive — and refuses with `*TableNewerError` — `ErrTableNewer` to `errors.Is`, naming the columns — running no DDL, a table carrying columns this engine does not know; a read or write that reaches an older table before `Ensure` has evolved it is the typed `ErrTableBehind`, the server's undefined-column error wrapped), `Save` takes the target's `TableLockSession`, confirms it from the write's own transaction, and writes the target's one row in one `INSERT … ON CONFLICT DO UPDATE` whose update is guarded on the row's format version and fingerprints and on a phase this engine knows — zero rows updated is an `IncompatibleError`, never a silent overwrite — and treats the shadow proof as write-once: a `Save` without a proof keeps the one the row holds, the same proof is accepted, and a different proof is `ErrInvariantViolation` that changes nothing, since a different shadow is a fresh start and goes through `Delete`; `Load` returns the row for a run with the same `Fingerprints`, `ErrNotFound` for no row, `ErrTableMissing` when `Ensure` never ran, an `IncompatibleError` naming the first disagreeing field (format, then source, then target fingerprint) and carrying the row's whole `Identity`, or, after bounded retries through transient errors (`dbconn.Retryable` plus a session the server ended from outside it, `57P01`/`57P02`/`57P03`) with an injected sleep, an error that is none of those; `Delete`, under the same lock, is the explicit fresh start and removes only the row whose `Identity` the caller was shown; `Inspect` is the resume's first read, before the run knows which statement it is resuming — the same outcomes as `Load`, guarded on the row format and the phase alone — a row in a phase this engine does not know is an `IncompatibleError` on `MismatchPhase`, since the phase set grows within a format without a version bump — so the row's fingerprints, phase, and shadow proof come back for the caller to judge, while every write stays guarded on the whole identity. The watermark column is `NULL` while nothing has landed, the LSN is a `pg_lsn`, the phase is stored by its stable name, and `shadow_proof` is the JSON proof of the shadow the run built (`Checkpoint.ShadowProof`, `NULL` until there is one), which a resume after the swap needs because `InspectShadow` cannot re-mint it once the shadow no longer bears its name; `PhaseReconciling`, between `catching_up` and `verifying`, is the ST-4 marker a reconcile writes before it touches the slot, so a reconcile that dies restarts as one. `ReapOrphanSlots` is the ST-3 reaper, placed here because the checkpoint row is what makes a slot owned: it lists the logical, engine-prefixed slots of `current_database()` and then reads the non-terminal rows that name them — a run makes its slot and commits the row that names it under `WithSlotLock`, the engine's advisory lock on the slot's name, transaction-scoped on a connection dialled outside the store's pool so the holder's own work on the pool never waits on the seat the lock sits in, so a slot with no row was left by a run that ended without dropping it, and a slot a done or failed row names by one whose cleanup did not finish — and re-reads each orphan's row right before dropping it, both under the same lock, so a run that claimed the slot since the pass began keeps it and one claiming it now waits for the drop and creates afresh; the pass drops each orphan through `decode.DropIdleSlot`, publication and all, reporting it in `Reaped.Dropped`; a slot a live row names (`Live`), a slot a walsender or backend holds when the drop comes round (`Active`), and a name wearing the prefix without the engine's shape (`Foreign`) are reported and left as found; slots of other databases are never read; the drops stop at the first failure, reporting what was dropped before it; a database where `Ensure` never ran is refused with `ErrTableMissing` rather than read as all orphans; and two reapers over one database take turns at each slot, the one that waited finding the slot gone. | ST-1, ST-2, ST-3, ST-4, LK-1 |
| `pkg/schemachange` | Shadow builder (`BuildShadow` produces `BuiltShadow`: source and shadow OIDs, fingerprints, identity handoff, copy columns, the statement's `USING` conversions, fidelity snapshot; before the retargeted statement runs it tries each conversion as a stored generated column's expression on the empty shadow — the one server-side check that admits only an immutable expression — in a save point it always rolls back, and refuses a non-immutable one as `shadow-statement-conversion` wrapping `ErrConversionNotImmutable`, since the copy, the recopy, and the catch-up evaluate the expression again at other times and must get the same answer; `SourceOfDerivedName` maps a derived name back to its table), orchestrator, and cutover. `BuildShadow`, `DropShadow`, and `InspectShadow` each take the `*dbconn.TableLockSession` for the table — a dedicated direct server session, distinct from the working pool, that `dbconn.AcquireTableLock` refuses to open through a transaction-pooling proxy: the operation runs under the session's `Bind` context so a lost lock cancels the statement in flight, and its transaction re-asserts from its own connection that the session's backend holds the lock before the first write, since the lock and the work are deliberately on different sessions. `InspectShadow` is the resume path `ErrShadowExists` points at: it re-derives the `BuiltShadow` proof from the catalog for the caller to compare with its checkpoint — `BuiltShadow.Proof()` is the plain, JSON-encodable view of that proof (`BuiltShadow` marshals as it), and nothing decodes back into a `BuiltShadow`, so only the builder and the inspection mint one; `DropShadow` is the D5 cleanup, dropping only a plain table the source's owner owns, without `CASCADE`. `GateCutover` is the ST-5 fidelity gate: handed a `BuiltShadow` and a `checksum.VerifiedShadow` for the same table, it re-reads both relations under the lock, refuses on a moved OID, a fingerprint or fidelity snapshot that drifted from the build's record, an invalid shadow index, or a swap name already taken, and otherwise mints `CutoverReady` — the pairing of every source index and extended statistics object with its shadow counterpart by catalog definition (not by name, since `LIKE` renames them; the default operator class and the column's own collation of a key column the statement retyped are set aside, since the server re-derives them for the new type; a written operator class or collation still has to agree, and a relaxed definition two indexes share pairs neither), and the sequences the swap must re-own; the swap itself consumes that proof. `Cutover` is the swap: one transaction under the lock session that takes `ACCESS EXCLUSIVE` on the source and the shadow under `lock_timeout` with bounded retry and backoff, runs the caller's `DrainFunc` (nil for a quiesced table) once per attempt once writers are excluded — a rolled-back attempt takes the drain's work with it, so the next attempt drains again from the same state — re-runs the copy-and-swap shape check (RF-2) and the gate's checklist so the pairing it renames by is as fresh as the lock, performs the D8 renames (every source dependent to its derived `_old` name, the source to `_old`, the shadow to the source's name, each paired shadow dependent to its partner's name), completes the D5 handoff (re-owning shared sequences; recreating each identity column with the source sequence's declared options — bounds following a widened column's type — under the source sequence's name, carrying the source sequence's grants, and `setval` to its `(last_value, is_called)`), re-reads the catalog to confirm every rename and handoff before committing, and mints `SwappedTable`; an attempt whose connection broke or whose context ended is resolved by following its backend to its exit and then reading from a fresh connection which OID bears the source name, never assumed (LK-4), and every attempt the catalog shows rolled back is reported wrapping `ErrCutoverRolledBack`. `DropOldTable` is the D9 drop: a separate bounded transaction that drops only the relation whose OID the `SwappedTable` recorded as the source, without `CASCADE`. `InspectSwapped` is the resume path between `Cutover` and `DropOldTable`, and the one home of LK-4's committed branch: handed the `Proof` the caller checkpointed before the cutover, it re-derives the `SwappedTable` from the catalog in one bounded read transaction under the lock session — the source's name must be borne by the proof's shadow OID and the derived `_old` name by the proof's source OID (ST-6), the dependent pairing is re-derived from the names the swap gave (a live dependent pairs with the old-table dependent under its derived name, so the re-derived pairs carry no shadow names), and every identity column the proof handed off must be an identity on the live table drawing from the sequence recreated under the source sequence's name; `SwappedTable.Proof()` is the plain, JSON-encodable `SwapProof` view of it (`SwappedTable` marshals as it), and nothing decodes back into a `SwappedTable`, so only the cutover and the inspection mint one. Both relations must still be owned by the role the build recorded, since the drop runs as it. The catalog states the proof does not describe each get their own answer: the source still live is `ErrNotSwapped` (run `Cutover`), the `_old` name borne by nothing while the source's OID is gone from the catalog is `ErrOldTableNotFound` (the drop already committed; the change is complete), the `_old` name borne by another relation — or by nothing while the retained source still exists under another name — is refused as `cutover-relation-replaced`, a live or old table owned by another role is refused as `cutover-swap-mismatch`, the source's name borne by neither relation is refused as `cutover-outcome-ambiguous` (LK-4), and so is a read while another backend holds either relation exclusively, since a swap whose `COMMIT` is still deciding holds both until its outcome is visible and the same call answers once it has let go. `SwapProof.SameSwap` is the resume's comparison: the two relations by OID and name, the owner, the re-owned sequences, and the identity columns, leaving out the dependent pairing and the attempt count a re-derived proof cannot carry. `DropOldTable` locks whatever bears the `_old` name before it reads the OID it compares, so the relation it proves is the relation it drops. `InspectResume` is the resume path's first read: handed a non-terminal checkpoint row for the target, it refuses one it cannot act on before any slot is touched — a terminal phase (`resume-row-terminal`), a row in `cutover`, which only `InspectSwapped` can answer (`resume-row-cutover`), another table or another slot name (`resume-row-mismatch`) — and otherwise answers `ResumeClean` when the slot is present, the server still serves it, and nothing holds it, or `ResumeReconcile` when the row names no slot, the slot is gone, the server reports it lost (with the server's cause on the `decode.SlotLostError` it carries), or the row is already in `reconciling`; a slot of the derived name that is physical or another database's is refused as `resume-slot-foreign` in every phase (it also answers `errors.Is` for `decode.ErrForeignDecodingState`), and the engine's own slot while another backend holds it as `resume-slot-held`, to be inspected again once the holder lets go; a target proven without logical decoding is always clean, having no slot to lose. `Reconciler.Reconcile` is the ST-4 reconcile mode (D11, D14), run whole under the lock session's `Bind` context so a lost lock cancels the slot command in flight — a drop still waiting out a holder included — and a session that has lost the table never drops or makes a slot a run holding the lock since may own: it applies `InspectResume`'s row checks, refuses a row whose shadow name or fingerprints are not the `BuiltShadow`'s, confirms from a transaction of its own that the source and the shadow are still the relations the build recorded (`cutover-relation-replaced` otherwise, before anything is written), writes the row to `reconciling` naming the derived slot and publication so the slot it is about to make is the row's from the moment it exists, replaces whatever slot bears the derived name with a fresh one through `decode.ReplaceSlot`, keeping the publication in place for the live source's writers, runs one `checksum` pass at or below the watermark under `recopy` when anything landed — recopying only the chunks the gap touched, with no reread, since the new slot carries every write that lands after the recopy — and writes the row back to `copying` or `catching_up` by the watermark, naming the new slot and its consistent point as the applied position the stream reopens from; the shadow is kept and the watermark never moves, and the open slot is returned for the caller to decode from or close. `Runner.Run` is the orchestrator, routed by the target's proof — a quiesced run for a target proven without logical decoding, a live run for one proven for it: it acquires the table lock session itself, makes sure the checkpoint table exists, reads the target's row through `Store.Inspect`, and dispatches on it — no row builds the shadow, fences the source, and writes `copying` with the statement's text (a relation already under the shadow's name with no row is refused as `shadow-unrecorded`; a first save whose row does not land drops the shadow and the fence again, so nothing unrecorded is left); a `copying` or `verifying` row is held against the statement it records — compared as parsed, the same operations on the same table, so spelling and schema qualification do not matter (`resume-row-mismatch` otherwise, and a row without a statement, or one whose statement names another table, likewise) — and against the shadow `InspectShadow` re-derives, names, fingerprints, and JSON-encoded proof alike (`resume-row-mismatch` otherwise), and installs over the fence the stopped run left; a row whose swap has not committed — a `cutover` row whose swap rolled back included — and whose shadow no relation bears is refused as `resume-shadow-gone`, the counterpart of `shadow-unrecorded`, writing nothing, so the row and the slot a live row names are left for the operator to drop and delete; a `cutover` row is answered by `InspectSwapped`, a committed swap continuing at the old table drop and `ErrNotSwapped` back through verify and gate; a terminal row is refused as `resume-row-terminal` and left for the caller to delete; each kind of run refuses the other kind's row as `resume-row-decoding` (the quiesced run a row that names a slot or stands in `catching_up` or `reconciling`, since it has no slot to continue it with; the live run a row that names no slot, since the source behind it was fenced, not captured) — then runs the phases in order. The quiesced run: the copier from the row's watermark, the landed watermark saved every `CheckpointInterval` and once more when the copy ends, `verifying` with the complete watermark, one `checksum` pass under `DivergenceAbort`, `GateCutover`, the row to `cutover` before `Cutover` is called with no drain, `DropOldTable` unless `KeepOld` — under which the fence, which rode the rename to `_old`, is taken off the retained table — and the row to `done`. The live run: no row creates the slot and writes `copying` naming it, with the slot's consistent point as the applied position, under `Store.WithSlotLock` so the reaper never meets the slot before its row (a first save that does not land drops the slot and the shadow again; a slot of the derived name the run did not make is reported as the `*decode.SlotExistsError` it is and left as found); a resumed row is read through `InspectResume` right before the stream opens, and a slot that is gone or lost is replaced by `Reconciler.Reconcile`, whose outcome `Result.Reconciled` and `Result.Reconciliation` report (a `cutover` row whose swap `InspectSwapped` shows rolled back is written back to `catching_up` first, since the inspection reads a `cutover` row as a swap still deciding); the stream is then opened at the row's position and applied to the shadow by a capture (`applier.Catchup` over a `Flusher`) that runs beside the fill — the copier while the row is in `copying`, its chunk positions handed to the capture so the stream's changes to a chunk land only once the chunk has; `catching_up` until the stream has confirmed past a marker written after the copy (a resumed `verifying` row catches up again before its pass); then `verifying`, one `checksum.CheckLive` pass under `DivergenceAbort` with the capture's `Catchup` as its confirmed source — then the capture is ended, the row records the position it confirmed through, `GateCutover`, the row to `cutover`, and `Cutover` with a drain that under the swap's lock writes a marker, reopens the stream from the row's position, gathers until the marker is delivered, closes the stream, waits for the slot to go idle — all on one connection dialled outside the pool for the attempt, since the swap holds a pool connection and a drain waiting on the pool for a second would wait on every other swap doing the same — and flushes everything gathered through the same `Flusher` (a batch the flusher defers is refused as `cutover-drain-deferred`; images the flusher held back are released and flushed once more, and a hold that persists, or anything left unapplied, is `ErrInvariantViolation`, since no writer can change the shadow under the swap's lock); the slot is dropped once the swap has committed, before `DropOldTable` or the `KeepOld` settle, and the row goes to `done`. The fence is the quiescence: a statement-level `BEFORE INSERT / UPDATE / DELETE / TRUNCATE` trigger on the source, created as the engine role, enabled `ALWAYS` so a session in `session_replication_role = replica` is refused too, and calling a function in the engine's schema, that refuses every write with SQLSTATE `55006` while the run is in progress or stopped between two saves (a resumed run installs over it, which enables it `ALWAYS` again); the quiesced `Runner`'s swap re-reads the fence under its `ACCESS EXCLUSIVE` lock and refuses as `cutover-unfenced` (CO-1) when the engine's trigger is not on the source, enabled `ALWAYS`, leaving the row in `cutover` for the next `Run` to install again and verify again (a live run installs no fence, so its swap does not check one); abandoning a stopped run is `DropShadow`, `DropWriteFence` (quiesced) or `decode.DropSlot` (live), and `Store.Delete` under the table lock. `PhaseFailed` is never written: a run that stops leaves the row at its last save and the next `Run` resumes from it. Every refusal the ten operations return is a `*RefusalError` carrying a `RefusalCause` from the closed set in [refusal-classes.md](refusal-classes.md#shadow-operation-refusals-keyed-on-refusalcause), so an importer routes on the cause rather than on message text; `Runner.Run` re-mints every `copier.RefusalCause` as a shadow cause too, wrapping the copier's refusal, so a caller reads one vocabulary whether the refusal came from a shadow operation or from the copy. | LK-1, LK-2, LK-4, CO-1, ST-4, ST-5, ST-6, ST-7 |
| `pkg/refusal` | `Class`, the routing class a typed refusal cause names — `capability-boundary`, `by-design`, `environmental`, or `invariant-violation`, the classes the cause tables of [refusal-classes.md](refusal-classes.md) use — answered by `copier.RefusalCause.Class()`, `schemachange.RefusalCause.Class()`, `preflight.PartitionRefusalCause.Class()`, `preflight.CopySwapRefusalCause.Class()`, and `executor.CreateShapeCause.Class()` from each cause's `class` cell, so an importer triages on the class without reading the cause's text; the per-cause row still decides the action, and a class never licenses a retry on its own. `verdict.Class` declares its tokens from this package, so the periphery converts with a plain conversion and the core never imports the verdict renderer. | — |

Each producing package owns its types. `pkg/schemachange` imports every producer; no producer
imports `pkg/schemachange`.

## Cutover transaction, step by step

1. Begin a bounded transaction, set `lock_timeout`, and acquire `ACCESS EXCLUSIVE` on the source
   and the shadow in one statement (the shadow lock excludes a straggling copier or applier
   connection); on `lock_timeout` roll back and retry with bounded backoff, never queue behind
   readers (LK-2).
2. Drain captured changes through the final WAL position — once per attempt, since a
   rolled-back attempt takes the drain's rows with it — then re-check the copy-and-swap shape
   (RF-2) and re-verify the `VerifiedShadow` and fidelity proofs already minted before the lock
   was taken; no checksum runs under the lock (CO-1, ST-5).
3. Rename the source's indexes, extended-statistics objects, and identity sequence to their
   deterministic `_old` names, the source to `_old`, and the shadow to the source name; then
   rename the shadow's indexes and statistics objects — paired with the source's by definition —
   to the names the source's held, restoring the constraint names with them (D8).
4. Complete the D5 sequence handoff: re-own a shared `serial`/`nextval` sequence to the live
   table; for identity, drop the shadow's `nextval` default, add identity with the source
   sequence's options, rename the new sequence to the name the source's identity sequence held,
   and `setval` it to the source's `(last_value, is_called)` read from the sequence relation.
5. Recheck catalog identities — live name, dependent names, sequence ownership — and commit. A
   lost connection is resolved by catalog inspection, never assumption (LK-4).
6. In separate bounded statements, remove the slot/publication and, unless `--keep-old` was set,
   drop the old table and, with it, the old identity sequence (D9).

## Deferred alternatives

| Alternative | Why deferred |
| --- | --- |
| Durable scratch database | Adds provisioning and privilege state without improving server-authoritative validation. |
| Post-copy index build | Needs its own resumable uniqueness, validity, and resource-control protocol. |
| Composite-key queue mode | Arbitrary-key ordering and watermark semantics require a separate applier design. |
| Deferred cutover | Adds a long-lived waiting state and extends slot/disk exposure. |
| Replica-lag throttle | Replica discovery and acceptable-lag policy are topology-specific. |
| Trigger-based capture | Adds synchronous write amplification and availability coupling; logical decoding is the v1 path. |
