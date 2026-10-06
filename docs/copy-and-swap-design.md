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
retarget re-verification, with the shadow as the sole permitted target).

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
format is refused, typed, and a fresh start deletes the row explicitly first. Creating the
schema needs `CREATE` on the database, the same privilege the scratch schema already needs.

**Why.** Ordinary table data follows the database through failover and gives each target one
atomic, local resume record.

**Alternative considered → deferred.** An external or append-only store introduces another
consistency boundary and unbounded history.

**Where enforced.** `pkg/checkpoint`; ST-1, ST-2.

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
formed. SHA-256 rather than `md5`: PostgreSQL built against OpenSSL routes `md5()` through it,
and an OpenSSL in FIPS mode refuses MD5, which would fail every pass on such a host; the digest
never leaves the process, so the hash has no compatibility surface.
Generated columns are absent from the copy list but included after the shadow recomputes them;
dropped columns are absent from the comparison.

**Why.** One comparison shape proves the stored post-change representation for widening,
narrowing, and precision changes instead of comparing unlike textual representations.

**Alternative considered → deferred.** Operation-specific checksum SQL multiplies semantic edge
cases and test surfaces.

**Where enforced.** `pkg/checksum` — the `Verifier`'s digest statement casts every copy column
to the type the shadow declares on both sides (the generated-column half is planned); CO-1, CO-2.

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
finding a `_pgsprite_…` relation they did not create.

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
checkpoint row is written and committed **before** the slot is created, so a slot with no row is
an orphan and never a slot mid-creation. Preflight checks free `max_replication_slots` and
`max_wal_senders` capacity. Slot lag has a hard byte ceiling, default 1 GiB; crossing it aborts
fail closed. At startup a reaper drops orphan `pgsprite_*` slots that belong to the current
database (`pg_replication_slots.database = current_database()`), are inactive
(`active_pid IS NULL`), and whose checkpoint row is absent or terminal; slots of other databases
are never inspected, let alone dropped. Slot inspection treats
`pg_replication_slots.wal_status = 'lost'` as lost state.

**Why.** A slot is durable cluster state that can retain unbounded WAL unless ownership and a
hard limit are explicit — and because it is cluster state, ownership must be established per
database, or a reaper in one database would push another's in-flight change into ST-4 slot loss.

**Alternative considered → deferred.** Best-effort cleanup alone cannot cover process death.

**Where enforced.** `pkg/decode`, `pkg/checkpoint`, and `pkg/preflight` (`CopySwapDecodingName` derives the name; `CheckCopySwapEnvironment` refuses the collision); ST-3, ST-4, ST-6.

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
floor and ceiling, so concurrent workers' reports do not compound; `Copier` times each chunk from claim to commit on an injected clock and feeds it back) and `pkg/decode`; LK-3, ST-3.

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
applier. With that discipline, a marker-bearing image for a key that did not move whose shadow
row is absent is a protocol error, not a case to handle: the row pre-exists on the source under
that key, so the key is either above the cut frontier (discarded under CO-4 at drain) or inside
an in-flight chunk (whose flush is deferred). The applier aborts the change fail closed if it
observes one. A moved image is different: its old key's shadow row exists only if the chunk
covering the old key was read **before** the move — a move captured while the old key was uncut,
or inside a chunk read after the move, leaves no shadow row under either key, and the buffer
cannot tell the two histories apart. The flush therefore completes a moved image from the old
key's shadow row when that row is present and otherwise from the **source** row under `Key`,
read in the flush's transaction; a source value read this way is the value the marker stood for
or a later one, and any later change arrives as a later event that overlays it, so the shadow
converges either way; an absent source row means a later event deletes or moves the key again,
and the flush skips the image rather than invent a value. The buffer keeps the image and the old
key's entry together across the drain (CO-4) so the old key's row is neither deleted nor copied
between completion and apply, and the drained `Batch` names those images in `CompleteFirst`:
its `Entries` are in key order, which puts a moved image after or before the delete marker at
its old key as the keys happen to sort, so the flush completes everything `CompleteFirst` names
before it writes anything, rather than reading the batch in slice order.
CO-6's second test vector therefore moves the primary key
of a row whose out-of-line column is untouched, and asserts the fallback completes it from the
old key's row rather than aborting; a third vector moves a row whose old key never landed and
asserts completion from the source.

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

**Decision.** The CLI accepts `--on-divergence=abort|repair` and defaults to `abort`. After slot
loss, resume internally selects `repair`, but only after a full re-verification.

**Why.** Ordinary divergence indicates a correctness defect; slot-loss reconciliation is the one
mode where known missing changes require repair. The mode must not be inferred from wiring.

**Alternative considered → deferred.** A universal repair default can conceal executor defects.

**Where enforced.** `pkg/checksum` `Check` takes a `DivergencePolicy` on every pass and refuses
the zero value (`ErrNoDivergencePolicy`); `abort` returns a `DivergenceError` with the shadow
untouched, `repair` recopies every differing chunk with the copier's own statement in one
transaction and reads each again, and a pass with repairs mints no proof. `pkg/schemachange` will select the policy per
lifecycle mode; CO-2, CO-3, ST-4.

### D15 — Capture changes with pgoutput

**Decision.** `pkg/decode` uses pinned `jackc/pglogrepl` with PostgreSQL's built-in `pgoutput`
plugin. Trigger capture remains the documented alternative in
[change-capture-tradeoff.md](change-capture-tradeoff.md), but is not built in v1.

**Why.** Logical decoding keeps synchronous work out of application writes, while pglogrepl owns
the load-bearing streaming-replication and pgoutput wire expertise.

**Alternative considered → deferred.** Trigger capture supports clusters without logical
decoding but adds write-path availability and amplification costs.

**Where enforced.** `pkg/decode` and `pkg/preflight`; ST-3, ST-4, ST-6.

## Package and proof-type map

| Package | Responsibility and proof types | Invariants |
| --- | --- | --- |
| `pkg/dbconn` | Produces `TableLock`, carried by `TableLockSession`; `Confirm` is the in-transaction check every writer runs from its own connection before its first write. | LK-1 |
| `pkg/preflight` | Produces `CopySwapShape` (the table facts `PreflightedTable` carries plus the v1 shape, replica identity, and dependent-object checks above, minted by `CheckCopySwapShape`) and from it `CopySwapTarget`, the copy-and-swap route's proof, minted only by `CheckCopySwapEnvironment` once the decoding and headroom facts are proven against that shape; `CheckCopySwap` runs the privilege, shape, and environment checks as one call; owns Tier-3 refusals. | ST-6, RF-1..RF-3 |
| `pkg/copier` | Produces `Chunk` and `Watermark`; `Chunker` (built only from a `CopySwapTarget`) cuts consecutive chunks that tile the whole int64 key space — first open below, last open above — so every key a row can carry belongs to exactly one chunk and a watermark at the largest value means the copy is complete. `Copier` (built from a `CopySwapTarget`, a `Shadow` — the shape `schemachange.BuiltShadow` satisfies — and the table's `TableLockSession`) copies chunks with several workers, each in its own bounded transaction under the owner's role with the catalog alone on its `search_path` that confirms the lock, takes `ACCESS SHARE` on both relations, and confirms both relation OIDs before one frozen never-overwriting insert — the chunk statement `pkg/internal/chunksql` holds for the copier and the verifier's repair alike; a resumed copy first deletes every shadow row above the watermark in batches of the same guarded shape — the whole shadow after a zero watermark — so the resumed cut frontier and the shadow agree on what is uncut, and its first batch takes `SHARE MODE` on the shadow so a chunk transaction of the earlier run still committing into it ends before the clear reads; the proof check refuses a shadow that is the source by name or by OID, since the clear deletes from one and the copy reads the other; the copy statement carries no conversion expression, so a column whose type differs between the two tables takes the server's assignment cast, and an `ALTER COLUMN TYPE … USING` change needs a conversion-aware copy before the planner's copy-and-swap route is wired to the copier; `Position` snapshots the cut frontier and landed watermark as two `Watermark` values plus the in-flight chunks, and `Position.Classify` is the applier's uncut / in-flight / landed rule. While `Run` is in progress the `Copier` is the `progress.Tracker`'s `WorkSource` (`Options.Tracker`): `rows_copied` is the rows this run's committed chunks inserted, `rows_total` the source's catalog row count read once at the start of the run, and `bytes_copied` / `bytes_total` the shadow's and the source's `pg_table_size` measured at each poll — every counter measured, none projected; on a resumed run the row ratio is that run's share, not the copy's completion. Each measurement, and each chunk cut, runs in a read-only transaction of its own under the chunk budgets with the catalog alone on its `search_path`, so a poll or a cut on a caller-built pool is still bounded (LK-2) and still resolves to the real catalog (CO-9); the copier stops being the source before `Run` returns. The copier's refusals are fail-closed `ErrInvariantViolation` values tagged with their invariant; they take a typed cause in [refusal-classes.md](refusal-classes.md#shadow-operation-refusals-keyed-on-refusalcause) when the orchestrator that runs the copy is wired, so the cause lands with its first importer. | CO-4, CO-9, LK-1, LK-2, LK-3 |
| `pkg/checksum` | `Verifier` (built from a `CopySwapTarget`, a `copier.Shadow`, and the table's `TableLockSession`) compares the source with its shadow up to the copier's landed watermark and returns a `Report` of the chunks that differ. It cuts its own chunks with a `copier.Chunker`, cutting and digesting each chunk in one read-only `REPEATABLE READ` transaction, so the cut and the chunk's two digests describe one snapshot, no snapshot outlives one chunk, and the cut's lock wait is bounded like the reads (LK-2); each transaction runs the copier's guard — owner role, catalog-only `search_path`, `ACCESS SHARE` on both relations taken before the snapshot, lock confirmation, relation-OID check — and pins `extra_float_digits` to its maximum, since the digest hashes each row's text rendering and a database or role configured at zero or below would render two floats that differ only in their last digits the same. Both sides run the identical frozen statement — row count plus hex `sha256` of the key-ordered concatenation of each row's `sha256(convert_to(ROW(col::shadow_type, …)::text, 'UTF8'))` over `pk BETWEEN $1 AND $2` (SHA-256 so a FIPS-mode OpenSSL, which refuses `md5()`, does not fail the pass) — so only the data can differ, and the cast on every column is D7: a converted column hashes as the value the shadow holds. Only the copy columns are compared; generated columns present on both sides are not yet hashed. A `Report` proves nothing. `Check` runs the same pass under a `DivergencePolicy` the caller states every time (the zero value is refused): `abort` returns a `DivergenceError` carrying the report with the shadow untouched; `repair` replaces every differing chunk inside one guarded read-write transaction — delete the shadow's rows over every chunk's key range, then the copy statement itself for every chunk, so a unique value the source moved from one differing chunk to another lands instead of colliding with the stale row — and digests each chunk again in a fresh snapshot, returning a `RepairError` for the first that still differs alongside the `Outcome` listing every repair that committed. A repair pass assumes nothing else writes the shadow and the source rows it recopies hold still until the rereads; a write inside the pass reads as a `RepairError`, never as a second repair. `ParseDivergencePolicy` lets a caller refuse a configured policy before any pass. Only a pass that found nothing and repaired nothing mints the proofs, whose constructors are private to the package: a `CleanWatermark` at the watermark it read through, and a `VerifiedShadow` only when that watermark is complete; `Outcome.Clean` is true only when the clean watermark was minted. A pass with repairs returns its `Repair`s and no proof; the next pass mints. While `Verify` or `Check` runs — through the repair phase as well — the verifier is the tracker's `progress.WorkSource` (`Options.Tracker`): `chunks_compared` and `rows_hashed` count the comparison's two-sided digests that have committed and the source rows they covered, `chunks_mismatched` the chunks the comparison found differing, `chunks_repaired` the chunks whose one recopy transaction has committed, and `chunks_reread` the repaired chunks digested again after it — every counter read from the pass's memory, none from the database, and all reset when a pass starts; a pass started while another runs on the same verifier is refused. | CO-1, CO-2, CO-3, CO-9, LK-1, LK-2 |
| `pkg/decode` | Produces `ChangeEvent`, including per-column presence and `OldKey` for an UPDATE that moved the primary key. | ST-3, ST-4, CO-4, CO-8 |
| `pkg/applier` | Merges events into the per-key buffer, drains it against the copier's `Position`, and applies the presence-aware batch. | CO-4, CO-5, CO-6, CO-8, LK-3 |
| `pkg/checkpoint` | Produces `Checkpoint` and persists it: `Store` (over a `pkg/dbconn` pool) creates `pgsprite.pgsprite_checkpoint` on first use (`Ensure`, serialized under the engine's advisory key so concurrent first users never race the create; it creates only what is absent, so a pre-provisioned schema and table the engine owns need no database `CREATE`, and refuses with `ErrForeignObject` a schema or table another role owns), `Save` takes the target's `TableLockSession`, confirms it from the write's own transaction, and writes the target's one row in one `INSERT … ON CONFLICT DO UPDATE` whose update is guarded on the row's format version and fingerprints — zero rows updated is an `IncompatibleError`, never a silent overwrite — `Load` returns the row for a run with the same `Fingerprints`, `ErrNotFound` for no row, `ErrTableMissing` when `Ensure` never ran, an `IncompatibleError` naming the first disagreeing field (format, then source, then target fingerprint) and carrying the row's whole `Identity`, or, after bounded retries through transient errors (`dbconn.Retryable` plus a session the server ended from outside it, `57P01`/`57P02`/`57P03`) with an injected sleep, an error that is none of those; `Delete`, under the same lock, is the explicit fresh start and removes only the row whose `Identity` the caller was shown. The watermark column is `NULL` while nothing has landed, the LSN is a `pg_lsn`, and the phase is stored by its stable name. | ST-1, ST-2, LK-1 |
| `pkg/schemachange` | Shadow builder (`BuildShadow` produces `BuiltShadow`: source and shadow OIDs, fingerprints, identity handoff, copy columns, fidelity snapshot; `SourceOfDerivedName` maps a derived name back to its table), orchestrator, and cutover. `BuildShadow`, `DropShadow`, and `InspectShadow` each take the `*dbconn.TableLockSession` for the table — a dedicated direct server session, distinct from the working pool, that `dbconn.AcquireTableLock` refuses to open through a transaction-pooling proxy: the operation runs under the session's `Bind` context so a lost lock cancels the statement in flight, and its transaction re-asserts from its own connection that the session's backend holds the lock before the first write, since the lock and the work are deliberately on different sessions. `InspectShadow` is the resume path `ErrShadowExists` points at: it re-derives the `BuiltShadow` proof from the catalog for the caller to compare with its checkpoint — `BuiltShadow.Proof()` is the plain, JSON-encodable view of that proof (`BuiltShadow` marshals as it), and nothing decodes back into a `BuiltShadow`, so only the builder and the inspection mint one; `DropShadow` is the D5 cleanup, dropping only a plain table the source's owner owns, without `CASCADE`. `GateCutover` is the ST-5 fidelity gate: handed a `BuiltShadow` and a `checksum.VerifiedShadow` for the same table, it re-reads both relations under the lock, refuses on a moved OID, a fingerprint or fidelity snapshot that drifted from the build's record, an invalid shadow index, or a swap name already taken, and otherwise mints `CutoverReady` — the pairing of every source index and extended statistics object with its shadow counterpart by catalog definition (not by name, since `LIKE` renames them; the default operator class and the column's own collation of a key column the statement retyped are set aside, since the server re-derives them for the new type; a written operator class or collation still has to agree, and a relaxed definition two indexes share pairs neither), and the sequences the swap must re-own; the swap itself consumes that proof. `Cutover` is the swap: one transaction under the lock session that takes `ACCESS EXCLUSIVE` on the source and the shadow under `lock_timeout` with bounded retry and backoff, runs the caller's `DrainFunc` (nil for a quiesced table) once per attempt once writers are excluded — a rolled-back attempt takes the drain's work with it, so the next attempt drains again from the same state — re-runs the copy-and-swap shape check (RF-2) and the gate's checklist so the pairing it renames by is as fresh as the lock, performs the D8 renames (every source dependent to its derived `_old` name, the source to `_old`, the shadow to the source's name, each paired shadow dependent to its partner's name), completes the D5 handoff (re-owning shared sequences; recreating each identity column with the source sequence's declared options — bounds following a widened column's type — under the source sequence's name, carrying the source sequence's grants, and `setval` to its `(last_value, is_called)`), re-reads the catalog to confirm every rename and handoff before committing, and mints `SwappedTable`; an attempt whose connection broke or whose context ended is resolved by following its backend to its exit and then reading from a fresh connection which OID bears the source name, never assumed (LK-4), and every attempt the catalog shows rolled back is reported wrapping `ErrCutoverRolledBack`. `DropOldTable` is the D9 drop: a separate bounded transaction that drops only the relation whose OID the `SwappedTable` recorded as the source, without `CASCADE`. Every refusal the six operations return is a `*RefusalError` carrying a `RefusalCause` from the closed set in [refusal-classes.md](refusal-classes.md#shadow-operation-refusals-keyed-on-refusalcause), so an importer routes on the cause rather than on message text. | LK-1, LK-2, LK-4, CO-1, ST-5, ST-6, ST-7 |

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
