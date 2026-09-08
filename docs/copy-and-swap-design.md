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
| Supported in v1 | Rewrite-requiring `ALTER COLUMN … TYPE`, initially `integer` → `bigint` identity primary keys, `text` → `varchar(n)`, and `numeric` precision/scale widening; volatile-default `ADD COLUMN`; and `STORED` generated-column addition. The table has one `smallint`, `integer`, or `bigint` primary-key column; PK-based `REPLICA IDENTITY DEFAULT` or `FULL`; no incoming or outgoing foreign keys, triggers, partitioning/inheritance, or rules; and every derived dependent-object name fits in PostgreSQL's `NAMEDATALEN` limit. |
| Typed refusal (planned) | Unsupported key (`copy-and-swap-pk-unsupported`); unsuitable replica identity (`copy-and-swap-replica-identity`); foreign keys (`copy-and-swap-foreign-keys`); triggers or rules (`copy-and-swap-triggers`); partitioned or inherited tables (`copy-and-swap-partitioned`); derived names longer than 63 bytes (`copy-and-swap-name-length`); unavailable logical decoding (`copy-and-swap-logical-decoding-unavailable`); insufficient replication-slot or WAL-sender capacity (`copy-and-swap-slot-headroom`); insufficient disk (`copy-and-swap-disk-headroom`); or insufficient grants (`copy-and-swap-grants`). Every refusal names its reason. |
| Out of scope | All other table shapes and operations, including primary-key changes, receive a typed refusal rather than an unsafe approximation. |

## Decisions

### D1 — No durable scratch database

**Decision.** Under `SET ROLE <owner>`, the executor creates the shadow in the source schema with
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

**Why.** The real server remains the semantic authority while the only durable temporary object is
the shadow needed by the operation itself.

**Alternative considered → deferred.** A separately provisioned durable scratch database adds
privilege and lifecycle requirements without strengthening the proof. Executing the statement
verbatim against a same-named shadow in an engine-owned schema reached through `search_path`
avoids the retarget but cannot handle a schema-qualified statement and would move the swap from
a rename to a `SET SCHEMA`; it was rejected for the two behaviours it would fork.

**Where enforced.** `pkg/schemachange` shadow builder and `pkg/schemadiff`; ST-2, ST-6.

### D2 — Build indexes and constraints up front

**Decision.** `INCLUDING ALL` carries indexes, constraints, defaults, comments, and storage
parameters when the shadow is created; identity is the D5 exception. Bulk copy pays index
maintenance as rows arrive.

**Why.** The shadow has its complete enforced shape throughout copy and reaches the fidelity gate
without a second long build phase.

**Alternative considered → deferred.** Building secondary indexes after copy may be faster, but
needs separate resumability, validity, uniqueness, and resource controls.

**Where enforced.** `pkg/schemachange` shadow builder and `pkg/preflight`; ST-5, ST-6.

### D3 — Store checkpoints in the target database

**Decision.** The engine role creates `pgsprite_checkpoint` in the target database on first use.
It contains one row per `(schema, table)`.

**Why.** Ordinary table data follows the database through failover and gives each target one
atomic, local resume record.

**Alternative considered → deferred.** An external or append-only store introduces another
consistency boundary and unbounded history.

**Where enforced.** `pkg/checkpoint`; ST-1, ST-2.

### D4 — Restrict the chunk key to one integer-family primary key

**Decision.** v1 accepts one `smallint`, `integer`, or `bigint` primary-key column. It uses
monotonic PK-range chunks and may discard captured events above the copier watermark under CO-4.
Composite, `uuid`, and text keys return `copy-and-swap-pk-unsupported`.

**Why.** A totally ordered, compact key makes chunk boundaries, resume watermarks, and the
watermark-discard proof share one representation.

**Alternative considered → deferred.** Queue-mode apply for arbitrary keys remains a later
extension.

**Where enforced.** `pkg/preflight`, `pkg/copier`, and `pkg/applier`; CO-4, ST-6.

### D5 — Hand off sequences and identity explicitly

**Decision.** For `serial` and explicit `DEFAULT nextval(...)` columns, `INCLUDING DEFAULTS`
causes source and shadow to share the source sequence; cutover runs
`ALTER SEQUENCE … OWNED BY <new-table>.<column>`. Identity metadata is excluded from the shadow.
The shadow instead receives `DEFAULT nextval('<source-identity-sequence>')`. While writers are
excluded by `ACCESS EXCLUSIVE`, cutover drops that default, recreates identity with
`ALTER TABLE … ALTER COLUMN … ADD GENERATED ALWAYS|BY DEFAULT AS IDENTITY`, and runs
`ALTER SEQUENCE <new-identity-sequence> RESTART WITH <source-last_value-plus-one>`, with
`last_value` read in the same transaction. Discovery uses `pg_get_serial_sequence` and verifies
the corresponding `pg_depend` ownership edge.

**Why.** Copy never advances an unrelated sequence, and the post-swap table preserves generation
kind with a next value beyond the source's observed state.

**Alternative considered → deferred.** Copying identity through `INCLUDING IDENTITY` creates an
independent sequence too early and obscures the state handoff.

**Where enforced.** `pkg/schemachange` shadow builder and cutover; ST-5.

### D6 — Preserve omitted TOAST values

**Decision.** v1 requires PK-based `REPLICA IDENTITY DEFAULT` or `FULL` and never changes the
user's replica identity. UPDATE apply is column-wise: only fields present in the pgoutput tuple
are assigned, so an unchanged TOASTed field omitted from the tuple remains untouched.

**Why.** Under the default replica identity, pgoutput can mark unchanged out-of-line values as
unchanged-TOAST rather than send their bytes. Treating absence as a value would corrupt the
shadow.

**Alternative considered → deferred.** Requiring or setting `REPLICA IDENTITY FULL` increases WAL
and mutates user configuration.

**Where enforced.** `pkg/decode` and `pkg/applier`; CO-8, ST-6.

### D7 — Checksum through the destination types

**Decision.** Each chunk computes `md5(string_agg(row_hash, '' ORDER BY pk))` on both sides.
Every compared source value is cast to the shadow column's type before its row hash is formed.
Generated columns are absent from the copy list but included after the shadow recomputes them;
dropped columns are absent from the comparison.

**Why.** One comparison shape proves the stored post-change representation for widening,
narrowing, and precision changes instead of comparing unlike textual representations.

**Alternative considered → deferred.** Operation-specific checksum SQL multiplies semantic edge
cases and test surfaces.

**Where enforced.** `pkg/checksum`; CO-1, CO-2.

### D8 — Use deterministic bounded names

**Decision.** Shadow and retained-source names are `_pgsprite_<8-hex-hash-of-schema.table>_new`
and `_pgsprite_<8-hex-hash-of-schema.table>_old`. Renamed indexes, constraints, and sequences on
the old table use the same suffix. Preflight returns `copy-and-swap-name-length` if any derived
identifier would exceed PostgreSQL's 63-byte `NAMEDATALEN` limit.

**Why.** Stable names make catalog inspection and resume deterministic; refusing truncation
prevents collisions.

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

**Decision.** Slot and single-table publication are both named `pgsprite_<8hex>`. Preflight checks
free `max_replication_slots` and `max_wal_senders` capacity. Slot lag has a hard byte ceiling,
default 1 GiB; crossing it aborts fail closed. At startup a reaper drops orphan `pgsprite_*`
slots whose checkpoint row is absent or terminal. Slot inspection treats
`pg_replication_slots.wal_status = 'lost'` as lost state.

**Why.** A slot is durable cluster state that can retain unbounded WAL unless ownership and a
hard limit are explicit.

**Alternative considered → deferred.** Best-effort cleanup alone cannot cover process death.

**Where enforced.** `pkg/decode`, `pkg/checkpoint`, and `pkg/preflight`; ST-3, ST-4, ST-6.

### D12 — Throttle by chunk time and slot lag

**Decision.** The copier adjusts chunk size toward a 500 ms target and also obeys D11's hard slot
lag ceiling.

**Why.** Time-targeted chunks adapt to row width and server capacity while keeping feedback and
checkpoint intervals bounded.

**Alternative considered → deferred.** Replica-lag-based throttling needs topology-specific
observation and policy.

**Where enforced.** `pkg/copier` and `pkg/decode`; LK-3, ST-3.

### D13 — Recover unique-secondary-key moves within the source transaction

**Decision.** A buffered flush is applied in source commit order, statement by statement, in one
transaction. On SQLSTATE `23505`, the batch is retried as delete-then-insert pairs.

**Why.** PostgreSQL's targeted `ON CONFLICT` cannot by itself move one unique secondary value
between rows, while preserving transaction order and retrying as replacement pairs converges.

**Alternative considered → deferred.** Ignoring secondary conflicts would leave convergence to
the checksum rather than the applier.

**Where enforced.** `pkg/applier`; CO-5, CO-6.

### D14 — Make divergence policy explicit

**Decision.** The CLI accepts `--on-divergence=abort|repair` and defaults to `abort`. After slot
loss, resume internally selects `repair`, but only after a full re-verification.

**Why.** Ordinary divergence indicates a correctness defect; slot-loss reconciliation is the one
mode where known missing changes require repair. The mode must not be inferred from wiring.

**Alternative considered → deferred.** A universal repair default can conceal executor defects.

**Where enforced.** `pkg/checksum` and `pkg/schemachange`; CO-2, CO-3, ST-4.

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
| `pkg/dbconn` | Produces `TableLock`. | LK-1 |
| `pkg/preflight` | Produces `CopySwapTarget`; owns Tier-3 refusals. | ST-6 |
| `pkg/copier` | Produces `Chunk` and `Watermark`. | CO-4, LK-3 |
| `pkg/checksum` | Produces `VerifiedShadow` and `CleanWatermark`; their constructors are private to this package. | CO-1, CO-2 |
| `pkg/decode` | Produces `ChangeEvent`, including per-column presence. | ST-3, ST-4, CO-4 |
| `pkg/applier` | Applies ordered, presence-aware events. | CO-4, CO-5, CO-6, CO-8 |
| `pkg/checkpoint` | Produces `Checkpoint`. | ST-1, ST-2 |
| `pkg/schemachange` | Orchestrator, shadow builder, and cutover. | LK-2, LK-4, ST-5 |

Each producing package owns its types. `pkg/schemachange` imports every producer; no producer
imports `pkg/schemachange`.

## Cutover transaction, step by step

1. Begin a bounded transaction, set `lock_timeout`, and acquire `ACCESS EXCLUSIVE` on the source;
   on `lock_timeout` roll back and retry with bounded backoff, never queue behind readers (LK-2).
2. Drain captured changes through the final WAL position and re-verify the `VerifiedShadow` and
   fidelity proofs already minted before the lock was taken — no checksum runs under the lock
   (CO-1, ST-5).
3. Rename source dependents to deterministic `_old` names, source to `_old`, and shadow to the
   source name; complete the D5 sequence and identity handoff.
4. Recheck catalog identities and commit. A lost connection is resolved by catalog inspection,
   never assumption (LK-4).
5. In separate bounded statements, remove the slot/publication and, unless `--keep-old` was set,
   drop the old table (D9).

## Deferred alternatives

| Alternative | Why deferred |
| --- | --- |
| Durable scratch database | Adds provisioning and privilege state without improving server-authoritative validation. |
| Post-copy index build | Needs its own resumable uniqueness, validity, and resource-control protocol. |
| Composite-key queue mode | Arbitrary-key ordering and watermark semantics require a separate applier design. |
| Deferred cutover | Adds a long-lived waiting state and extends slot/disk exposure. |
| Replica-lag throttle | Replica discovery and acceptable-lag policy are topology-specific. |
| Trigger-based capture | Adds synchronous write amplification and availability coupling; logical decoding is the v1 path. |
