# Change-capture trade-off: triggers vs logical decoding (copy-and-swap)

Both approaches build the **same** copy-and-swap migration (shadow table → chunked copy →
catch-up → checksum → atomic swap). They differ only in **how concurrent writes are captured**
during the copy, and that single choice cascades into very different operational properties. This
doc is the canonical comparison; **any doc that proposes logical-decoding copy-and-swap as the
default must point here**, because the right default is cluster-dependent, not absolute.

> TL;DR: **logical decoding is the default** (near-zero source overhead) and **triggers are the
> robustness fallback** (survive failover, work anywhere). Neither lets us drop the **mandatory
> checksum**; triggers do let us drop the fragile replication-slot checkpoint, but not the
> copy-watermark checkpoint.

## Table of contents

- [The two approaches in one line each](#the-two-approaches-in-one-line-each)
- [Side-by-side comparison](#side-by-side-comparison)
- [Does either approach let us drop the checksum or the checkpoint?](#does-either-approach-let-us-drop-the-checksum-or-the-checkpoint)
- [When to default to each](#when-to-default-to-each)
- [Decision: primary + fallback, behind one interface](#decision-primary--fallback-behind-one-interface)

## The two approaches in one line each

- **Logical decoding (log-based).** Read committed changes off the WAL via a replication slot —
  no code in the write path. The faithful analog of Spirit/gh-ost reading the MySQL binlog. This
  is the **differentiator**: no PostgreSQL OSS OSC tool does this today (they all use triggers).
- **Triggers (synchronous capture).** Put `AFTER INSERT/UPDATE/DELETE` triggers on the source so
  every write also records the change (directly into the shadow, or into a durable queue table
  drained by the applier). The approach pg_osc and pt-online-schema-change use.

## Side-by-side comparison

| Dimension | Logical decoding (default) | Triggers (fallback) |
| --- | --- | --- |
| **Source write overhead** | ✅ ~none — capture is off the WAL, outside the write txn | ❌ write amplification — every write does extra work *inside* the txn, plus lock/deadlock risk on hot tables |
| **Prerequisites** | `rds.logical_replication=1` (**reboot**), `rds_replication` role, a replication-protocol connection (no RDS Proxy), `REPLICA IDENTITY` | none — ordinary `TRIGGER` privilege; works on any cluster, no reboot, no params |
| **Survives Aurora failover?** | ⚠️ **no, not guaranteed** — the slot lives on the writer and Aurora doesn't sync it; failover can lose it (see slot loss on failover) | ✅ **yes** — the trigger + queue/shadow are ordinary data, replicated by Aurora storage |
| **WAL / disk-retention risk** | ❌ an abandoned/slow slot pins WAL and can fill the volume | ✅ none (queue-table bloat is a normal, vacuumable concern) |
| **Capture completeness** | ⚠️ wrinkles: unchanged-TOAST omitted unless `REPLICA IDENTITY FULL`; generated cols arrive NULL; DDL not decoded (must lock out concurrent DDL) | ✅ trigger sees the **full new row** synchronously — no TOAST/generated-col gaps |
| **Snapshot ↔ position coordination** | ❌ must seed the copy from the slot's exported snapshot and replay strictly from that LSN | ✅ sidestepped — trigger captures from creation; copy + capture reconcile |
| **Throughput of catch-up** | commit-ordered, effectively serial (mitigated by PG14 streaming / PG16 parallel apply — see [postgresql-version-support](postgresql-version-support.md#the-version-we-pivot-on)) | bounded by trigger/queue apply; also serial-ish, but no slot/LSN machinery |
| **Pause / resume of capture** | ✅ easy — just stop/start consuming the slot | ⚠️ harder — triggers fire regardless; a queue-table design can defer apply, direct-to-shadow cannot |
| **Cutover lock profile** | brief `ACCESS EXCLUSIVE` swap (same for both) | brief swap **plus** trigger creation/drop takes a momentary strong lock |
| **Version floor** | PG 14 for sane large-txn behaviour (our floor anyway) | works far back (pg_osc reaches 9.6) |
| **Net** | low steady-state cost, higher operational complexity + failover fragility | high steady-state cost, much simpler failure model |

## Does either approach let us drop the checksum or the checkpoint?

This is the crux, and the honest answer is **no for the checksum, partly yes for the checkpoint**.

**Checksum stays mandatory either way.**
- It is a **mechanism-independent** gate: copy correctness (chunk boundaries, type coercion,
  collation, generated columns) has nothing to do with how changes are captured.
- Triggers remove the *logical-decoding* failure modes (TOAST omission, DDL desync, slot-gap on
  failover) but add **their own**: the classic trigger-ordering race where a `DELETE`/`UPDATE`
  trigger fires *before* that row has been copied into the shadow, silently diverging the two
  tables. pt-online-schema-change has hit exactly this in production.
- So both mechanisms can produce a diverged shadow by different routes. The checksum is the single
  place we *prove* equality before the swap — it is non-negotiable in either world. This is the
  [mandatory-checksum design principle](design-principles.md), unchanged.

**Checkpoint/resume is simplified by triggers, not eliminated.**
- A copy-and-swap has **two** kinds of durable progress: the **copied-PK watermark** (how far the
  bulk copy got) and the **change-capture position**.
- With **logical decoding**, the capture position is the slot's `confirmed_flush_lsn` — *fragile*
  state that must be persisted and that an Aurora failover can destroy, forcing a checksum-repair
  reconciliation (see [low-level-design's failover analysis](low-level-design.md#failover-during-migration-what-survives-and-what-doesnt)).
- With **triggers**, the capture is *durable by construction*: a queue table's drain position is
  ordinary data, and a direct-to-shadow trigger keeps the shadow continuously live. There is no
  slot LSN to checkpoint and **no slot-loss-on-failover restart risk** — the migration simply
  resumes. That removes the scariest part of the checkpoint design.
- **But** the **copy-watermark checkpoint is still needed** in both cases: a multi-hour copy of a
  large table must resume from where it stopped rather than re-copy from row 0.

So triggers buy a dramatically simpler *failure model* (no slot, no LSN coordination, survives
failover), at the cost of source write overhead — and they do **not** buy you out of the checksum.

## When to default to each

| Situation | Prefer |
| --- | --- |
| Hot, write-heavy table where trigger amplification is unacceptable; logical replication is enabled | **Logical decoding** |
| Cluster where `rds.logical_replication` can't be enabled (no reboot window) or the role/connection constraints can't be met | **Triggers** |
| Multi-day migration on a cluster with realistic failover/maintenance exposure | **Triggers** (failover-safe) — or logical decoding *with* a tested checksum-repair resume |
| Short migration on a quiet table | either; logical decoding has less footprint |
| Sharded fleet running N migrations at once (slot/WAL pressure per cluster) | weigh per-cluster slot budget — see sharded-aurora-postgresql |

## Decision: primary + fallback, behind one interface

When the copy-and-swap backend is built, it will define a **single change-capture abstraction**
with two implementations selected per migration; no change-capture code exists yet:
**logical decoding as the default** (the low-overhead differentiator) and **triggers as a
first-class fallback** (the failover-safe, runs-anywhere path) — not a vestige. The planner picks
based on cluster facts (is logical replication enabled? failover exposure? table write rate?), and
the rest of the pipeline (chunked copy, **mandatory checksum**, copy-watermark checkpoint, atomic
cutover) is **identical** regardless of which capture is chosen. See
[the change-capture decision in low-level-design](low-level-design.md#1-cdc-mechanism--logical-decoding-with-trigger-fallback).
