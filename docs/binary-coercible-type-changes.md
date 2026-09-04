# Type changes without a rewrite: binary coercibility in PostgreSQL

An `ALTER TABLE … ALTER COLUMN … TYPE` is either a catalog relabel that finishes in
milliseconds or a full table rewrite under `ACCESS EXCLUSIVE` — same syntax, opposite cost.
This document explains how PostgreSQL makes that decision, the four shapes of change that
skip the rewrite and *why* they are safe to skip, every category of reason a rewrite is
forced, the changes that look free but are not, and what "no rewrite" still costs you.

```sql
-- varchar(20) → text: same on-disk bytes, catalog-only, milliseconds
ALTER TABLE orders ALTER COLUMN sku TYPE text;

-- integer → bigint: 4 bytes → 8 bytes, every row re-encoded, every index
-- on the column rebuilt, all under ACCESS EXCLUSIVE
ALTER TABLE orders ALTER COLUMN amount TYPE bigint;
```

Everything here applies to PostgreSQL 14 and later (the versions pg-sprite supports). The
mechanics are cited to the PostgreSQL source in [Source pointers](#source-pointers) so a
claim can be re-checked rather than trusted.

## Table of contents

- [Terms used in this document](#terms-used-in-this-document)
- [The rule in one sentence](#the-rule-in-one-sentence)
- [How PostgreSQL decides](#how-postgresql-decides)
  - [What a function cast is](#what-a-function-cast-is)
- [The four shapes that skip the rewrite](#the-four-shapes-that-skip-the-rewrite)
  - [1. Binary-coercible casts (`pg_cast.castmethod = 'b'`)](#1-binary-coercible-casts-pg_castcastmethod--b)
  - [2. Typmod relaxation (same type, looser modifier)](#2-typmod-relaxation-same-type-looser-modifier)
  - [3. Unconstrained domains over the same base type](#3-unconstrained-domains-over-the-same-base-type)
  - [4. `timestamp` ↔ `timestamptz` under a UTC session](#4-timestamp--timestamptz-under-a-utc-session)
- [Why rewrites happen: the six categories](#why-rewrites-happen-the-six-categories)
  - [Things that are *not* rewrite reasons](#things-that-are-not-rewrite-reasons)
- [Looks free, but rewrites](#looks-free-but-rewrites)
  - [Shortening `varchar(n)`](#shortening-varcharn)
  - [`numeric(10,2)` → `numeric(10,3)`: the value survives, the datum does not](#numeric102--numeric103-the-value-survives-the-datum-does-not)
  - [`numeric(12,2)` → `numeric(10,2)`: the rows fit, PostgreSQL cannot know](#numeric122--numeric102-the-rows-fit-postgresql-cannot-know)
- [No rewrite is not no cost](#no-rewrite-is-not-no-cost)
- [Checking before you run it](#checking-before-you-run-it)
  - [Failing closed with a `table_rewrite` event trigger](#failing-closed-with-a-table_rewrite-event-trigger)
- [How pg-sprite classifies type changes](#how-pg-sprite-classifies-type-changes)
- [Source pointers](#source-pointers)

## Terms used in this document

| Term | Meaning |
| --- | --- |
| **Type** | The declared data type of a column: `integer`, `text`, `numeric`, … Determines the on-disk encoding of each value. |
| **Type modifier (typmod)** | The parenthesised part of a type: the `50` in `varchar(50)`, the `(10,2)` in `numeric(10,2)`, the `3` in `timestamp(3)`. It constrains values; it never changes the storage format. `-1` means "unconstrained". |
| **Datum** | The bytes of one stored value. A **rewrite** exists to produce new datums for every row. |
| **Cast** | A conversion from one type to another, described by a row in `pg_cast`. |
| **`castmethod`** | How the cast is performed: `b` = *binary-compatible* (no function, the bytes are reinterpreted), `f` = *function* (a C or SQL function transforms each value), `i` = *I/O conversion* (the value is rendered to text and re-parsed by the target type). |
| **Relabel** | The planner node (`RelabelType`) that changes a value's type label without touching its bytes. The output of a binary-compatible cast. |
| **Transform expression** | The expression PostgreSQL builds to turn the old column value into the new one. Its *shape* after simplification is what decides whether a rewrite happens. |
| **Planner support function** | A per-type hook (`varchar_support`, `numeric_support`, …) the planner consults to simplify calls of that type's functions — including proving a length check is a no-op. |
| **Domain** | A named type built on a base type, optionally with `CHECK` / `NOT NULL` constraints: `CREATE DOMAIN email AS text CHECK (VALUE ~ '@')`. Stored exactly like its base type. |
| **Rewrite** | PostgreSQL copies every row into a new physical file (a new *relfilenode*), rebuilding all indexes, while holding `ACCESS EXCLUSIVE` on the table. |

## The rule in one sentence

PostgreSQL rewrites the table unless it can prove, **from the shape of the transform
expression alone**, that every byte already on disk is a valid datum of the new type. It
never scans the data to find out. A change that would be harmless for the rows you happen to
have still rewrites if the *type system* cannot prove it harmless for every possible row.

## How PostgreSQL decides

`ATPrepAlterColumnType` (`src/backend/commands/tablecmds.c`) builds the transform expression
— the `USING` clause if you gave one, otherwise a bare reference to the column — and coerces
it to the target type with assignment-cast rules. It then runs the expression through the
planner's simplifier (`expression_planner`), which is where the type-specific planner support
functions get a chance to prove a length coercion is a no-op and replace it with a relabel.

`ATColumnChangeRequiresRewrite` then walks the simplified expression:

```
loop over the expression:
  a plain reference to the column           → NO REWRITE      (stop)
  RelabelType                               → strip it, continue
  CoerceToDomain  with no domain constraints → strip it, continue
                  with CHECK / NOT NULL      → REWRITE
  FuncExpr  timestamp ↔ timestamptz, and the session time zone is a fixed +00:00
                                            → strip it, continue
            any other function              → REWRITE
  anything else (ArrayCoerceExpr, CoerceViaIO, …) → REWRITE
```

Two consequences follow. First, the decision is **structural**: if a real function call
survives simplification, the table rewrites, however cheap that function would be per row.
Second, the decision depends on the column's **current** type, which is not in the DDL text
— `ALTER COLUMN sku TYPE text` is free if `sku` is `varchar(20)` and a rewrite if it is
`integer`. Anything that classifies these statements ahead of time has to introspect the
catalog first.

### What a function cast is

The value `42` stored as `integer` is four bytes; stored as `bigint` it is eight:

```
integer  42 →  2A 00 00 00
bigint   42 →  2A 00 00 00 00 00 00 00
```

There is no way to read the four-byte datum as an eight-byte one — every row must pass
through the C function `int8(integer)`, which reads four bytes and writes eight, and the
row layout of every tuple grows. That function call is the `FuncExpr` the rewrite test
refuses to strip. Contrast `varchar → text`: both are stored as `[length header][bytes]`,
so the planner only changes the type label on the same datum — a `RelabelType`.

The catalog says which is which:

```sql
SELECT castsource::regtype, casttarget::regtype, castfunc::regproc, castmethod
FROM   pg_cast
WHERE  (castsource, casttarget) IN (('integer'::regtype, 'bigint'::regtype),
                                    ('varchar'::regtype, 'text'::regtype));

--     castsource     | casttarget | castfunc | castmethod
-- -------------------+------------+----------+-----------
--  integer           | bigint     | int8     | f          ← function: rewrite
--  character varying | text       | -        | b          ← binary: relabel
```

Casts to and from string types that have no `pg_cast` row at all (`integer → text`,
`text → uuid`) are performed by *I/O conversion*: render with the source type's output
function, parse with the target's input function. The planner represents these as
`CoerceViaIO`, which the rewrite test also refuses to strip.

## The four shapes that skip the rewrite

### 1. Binary-coercible casts (`pg_cast.castmethod = 'b'`)

Two distinct types whose values share one on-disk representation. The catalog keeps them
separate because they differ in semantics, I/O functions, or operators — but the stored
bytes are identical, so the planner emits a `RelabelType` and the loop strips it.

| Cast | Why the bytes are identical |
| --- | --- |
| `varchar` → `text`, `text` → `varchar` (unbounded) | Both are variable-length strings; the `varchar` length limit is a check, not a storage format |
| `xml` → `text` | `xml` is stored as text; only the input validation differs |
| `cidr` → `inet` | Same struct; `cidr` is `inet` with a stricter invariant (no host bits) |
| `oid` ↔ `regclass`, `regtype`, `regproc`, … | All 4-byte object identifiers with different display functions |

Ask the catalog rather than memorising the list:

```sql
SELECT castsource::regtype, casttarget::regtype
FROM   pg_cast
WHERE  castmethod = 'b';
```

Binary coercibility is **directional**. `cidr → inet` is a relabel; `inet → cidr` masks
the host bits and is a function cast. `xml → text` is a relabel; `text → xml` validates
the document and is a function cast. Check the row for the direction you are changing.

### 2. Typmod relaxation (same type, looser modifier)

`varchar(50) → varchar(100)` is not a cast at all — the type is `varchar` on both sides
and only the modifier changes. The coercion still starts life as a function call
(`varchar(value, 100)`, the length check), but during simplification the type's planner
support function proves the check can never reject or alter a value the old modifier
admitted, and replaces the call with a `RelabelType`.

Each type with a support function encodes its own "cannot lose information" rule:

| Type | Support function | Relabel (no rewrite) when |
| --- | --- | --- |
| `varchar(n)` | `varchar_support` | new limit is unbounded, **or** old was bounded and new ≥ old |
| `numeric(p,s)` | `numeric_support` | new is unconstrained, **or** both constrained with the **same scale** and new precision ≥ old |
| `varbit(n)` | `varbit_support` | new limit is unbounded, **or** old was bounded and new ≥ old |
| `timestamp(p)`, `timestamptz(p)`, `time(p)`, `timetz(p)` | `timestamp_support` etc. | new precision is unspecified or the type's maximum, **or** old was specified and new ≥ old |
| `interval` fields/precision | `interval_support` | new range keeps every field the old one allowed and (if seconds are in range) fractional precision does not shrink |

Strictly, this is not binary coercion in PostgreSQL's own vocabulary — that term is for the
`castmethod = 'b'` rows above — it is a **no-op typmod coercion**. The operational outcome
is the same (catalog-only, no scan, no rewrite), which is why pg-sprite reports both under
one verdict reason.

### 3. Unconstrained domains over the same base type

A domain is a named type layered on a base type:

```sql
CREATE DOMAIN sku_code AS text;                          -- unconstrained
CREATE DOMAIN email    AS text CHECK (VALUE ~ '@');      -- constrained
```

A domain has **no storage of its own**. A `sku_code` value is stored byte-for-byte as a
`text` value; the domain only adds checks that run when a value is *assigned*. So changing
a `text` column to `sku_code` produces a `CoerceToDomain` node with nothing to enforce, and
the rewrite test strips it — the existing bytes are already valid `sku_code` datums.

```sql
ALTER TABLE products ALTER COLUMN code TYPE sku_code;   -- text → unconstrained domain: relabel
ALTER TABLE users    ALTER COLUMN mail TYPE email;      -- text → constrained domain: REWRITE
```

The second statement rewrites because every existing row would have to be checked against
`VALUE ~ '@'`, and PostgreSQL has no "scan without rewrite" path for `ALTER COLUMN TYPE`
— its only tool for "check every row" is to re-create every row. `NOT NULL` on the domain
counts as a constraint too; a `DEFAULT` does not (defaults never affect existing rows).

Three related facts:

- **Domain → its base type** (or dropping a domain in favour of `text`) is always a
  relabel. There is nothing to check when you *remove* constraints.
- **The base type may itself be relabelled first.** `varchar(20) → sku_code` is
  `varchar → text` (binary-coercible) followed by `CoerceToDomain` (unconstrained): both
  strip, no rewrite.
- **Stacked domains** behave the same way: each `CoerceToDomain` layer is stripped if
  that domain has no constraints, and forces a rewrite if it has any.

### 4. `timestamp` ↔ `timestamptz` under a UTC session

Converting between `timestamp` and `timestamptz` normally depends on the session
`TimeZone` and is a function cast. PostgreSQL 12+ special-cases exactly these two functions
in the rewrite test: if the session time zone is a fixed zero offset (`UTC`, `Etc/UTC`,
`+00`), the conversion is the identity on the stored 8-byte value, so it is treated as a
relabel.

```sql
SET timezone = 'UTC';   -- the same ALTER under 'Australia/Sydney' rewrites the table
ALTER TABLE events ALTER COLUMN created_at TYPE timestamptz;
```

This is the one case where the rewrite decision depends on **session state**, not on the
catalog. A tool that classifies the change ahead of time has to know which time zone the
executing session will run under.

## Why rewrites happen: the six categories

A rewrite happens whenever a node the test cannot strip survives simplification. Grouping
by *why* the node survives gives six categories; every rewriting type change falls into at
least one. The letter tags are used in the [Looks free, but rewrites](#looks-free-but-rewrites)
table.

| # | Category | Why the bytes cannot be reused | Example |
| --- | --- | --- | --- |
| **A** | **Different on-disk representation** | The two types encode values differently (width, layout, or structure), so a function must produce every new datum. | `integer → bigint` (4 → 8 bytes); `real → double precision`; `integer → numeric`; `json → jsonb` (text → binary tree); `uuid → text` |
| **B** | **Value-transforming conversion** | The types are "similar", but the cast or coercion function changes the bytes of at least some values — padding, masking, normalisation, rounding. | `char(10) → text` (`rtrim1()` strips the padding); `inet → cidr` (masks host bits); `char(10) → char(20)` (the padding is stored, so every datum grows); `timestamp(6) → timestamp(3)` (rounds fractional seconds — `TemporalSimplify` only relabels when precision does not shrink); `interval` precision reduction (same, via `interval_support`) |
| **C** | **A modifier stored inside the datum changes** | The typmod is not just a check — part of it is written into each value's header, so a different typmod means a different datum even when the value is equal. `numeric` is the one built-in type that does this. | `numeric(10,2) → numeric(10,3)` (display scale lives in the datum — see [below](#numeric102--numeric103-the-value-survives-the-datum-does-not)) |
| **D** | **A tightening the type system cannot prove safe** | The new type or modifier could reject a value the old one accepted. PostgreSQL does not scan to see whether any row *would* be rejected; it rewrites, and fails if one is. | `varchar(100) → varchar(50)`; `text → varchar(255)`; `numeric(12,2) → numeric(10,2)`; `text → xml` (validation); `text → email` (domain with `CHECK` or `NOT NULL`); `varbit(16) → varbit(8)` |
| **E** | **Session-dependent conversion** | The result depends on runtime state, so the bytes are only reusable under one specific setting. | `timestamp → timestamptz` under any session time zone other than a fixed +00:00 |
| **F** | **Wrapped in a node the rewrite test does not recognise** | The conversion may be harmless, but it is expressed as a node type the test has no case for, so it falls through to "rewrite". | `varchar(50)[] → varchar(100)[]` (`ArrayCoerceExpr` — the scalar rule is not lifted to arrays); `integer → text` (`CoerceViaIO`); any `USING` clause containing a real function call, e.g. `USING lower(col)` or `USING col || ''` |

Categories A–C are *genuine*: the datums really must change. Category D is *conservative*:
the datums might all be fine, but PostgreSQL will not look. Categories E and F are
*mechanical*: the bytes are reusable in principle, but the decision procedure cannot see it.
Knowing which category a change falls into tells you whether there is an online alternative
(D often has one — see the `varchar` shortening idiom below; A never does).

### Things that are *not* rewrite reasons

Common assumptions that turn out to be wrong in the safe direction:

- **Indexes on the column.** They are dropped and re-created by the statement, and rebuilt
  if incompatible, but they never cause a *heap* rewrite. See
  [No rewrite is not no cost](#no-rewrite-is-not-no-cost).
- **Constraints referencing the column** (`CHECK`, `UNIQUE`, foreign keys). Same: re-created
  in the statement, not a heap rewrite.
- **An `interval` field-only reduction** (`DAY TO SECOND` → `HOUR TO SECOND`, for example).
  The transform is a bare relabel with no heap rewrite, and existing values keep components
  the new declared type excludes — a day component can survive in an `interval HOUR TO SECOND`.
- **A collation change alone** (`ALTER COLUMN name TYPE text COLLATE "C"` on a `text`
  column). The transform is a bare relabel — no heap rewrite — but every index on the column
  is rebuilt because its sort order may differ.

## Looks free, but rewrites

Each of these is a reasonable guess that fails the structural test. The tag refers to the
[category table](#why-rewrites-happen-the-six-categories).

| Change | Why it looks free | What actually happens | Why | Tag |
| --- | --- | --- | --- | --- |
| `varchar(100)` → `varchar(50)` | "every value already fits" | full rewrite + reindex under `ACCESS EXCLUSIVE`; errors if any row is too long | `varchar_support` only simplifies when new ≥ old; the length check survives and PostgreSQL does not scan to see whether it would pass — see [below](#shortening-varcharn) | D |
| `text` → `varchar(255)`, `varchar` → `varchar(255)` | "just adding a limit" | full rewrite | the old typmod is unbounded, so the "old was bounded" guard fails and the check survives | D |
| `numeric(10,2)` → `numeric(10,3)` | "wider" | full rewrite | the display scale is stored in every datum; a scale change alters every stored value — see [below](#numeric102--numeric103-the-value-survives-the-datum-does-not) | C |
| `numeric(12,2)` → `numeric(10,2)` | "same scale, my values fit" | full rewrite; errors if any row overflows | precision shrinks; PostgreSQL will not look at the rows — see [below](#numeric122--numeric102-the-rows-fit-postgresql-cannot-know) | D |
| `integer` → `bigint`, `smallint` → `integer`, `real` → `double precision`, `integer` → `numeric` | "widening within the family" | full rewrite + every index rebuilt | different width or encoding; the cast is a real function (`int8(integer)`) | A |
| `char(n)` → `text` / `varchar` | "all text types" | full rewrite | `bpchar → text` is `rtrim1()` — it strips the padding — a function cast, not a relabel. `char(n) → char(m)` rewrites too: the padding is stored | B |
| `varchar(50)[]` → `varchar(100)[]` | "element widening is free" | full rewrite | the coercion is an `ArrayCoerceExpr`; the rewrite test has no case for it, and support-function simplification applies to scalars only | F |
| `text` → domain with `CHECK` or `NOT NULL` | "only adds validation" | full **rewrite**, not a validation scan | `DomainHasConstraints` → rewrite; there is no scan-only path for `ALTER COLUMN TYPE` | D |
| `timestamp` → `timestamptz` under a non-UTC session | "PG 12 made this free" | full rewrite | the exemption applies only when the session zone is a fixed +00:00 | E |
| `inet` → `cidr`, `text` → `xml` | "the reverse direction is a relabel" | full rewrite | binary coercibility is directional; these directions are function casts | B, D |
| `json` → `jsonb`, `uuid` ↔ `text`, `integer` → `text` | "lossless" | full rewrite | different storage formats; the casts go through functions or I/O conversion | A, F |
| same type, only `COLLATE` changes | "nothing about the data changes" | **no heap rewrite**, but every index on the column is rebuilt | the transform is a bare relabel, but the index's collation changes and its storage cannot be reused — the sort order may differ | — |

### Shortening `varchar(n)`

The most common surprise. There is little practical reason to shorten a `varchar` limit,
but for completeness: `varchar(100) → varchar(50)` is a **full table rewrite**, not a
check. PostgreSQL re-encodes every row through the length-coercion function, rebuilds every
index on the column, and holds `ACCESS EXCLUSIVE` for the whole duration; if any row is
longer than 50 characters the statement fails after doing that work (`value too long for
type character varying(50)`). Widening is free because the type system can prove nothing is
lost; shortening cannot be proven from the type alone, and PostgreSQL's answer to "cannot
prove" is always "rewrite", never "scan and see".

If the goal is to *enforce* a shorter limit rather than to change the declared type, the
online idiom is a constraint:

```sql
ALTER TABLE t ADD CONSTRAINT t_col_len CHECK (length(col) <= 50) NOT VALID;  -- brief lock
ALTER TABLE t VALIDATE CONSTRAINT t_col_len;   -- SHARE UPDATE EXCLUSIVE, concurrent DML OK
```

Same guarantee for new writes and, after `VALIDATE`, for existing rows — with the declared
type left as `varchar(100)`. If the declared type itself must change, that is a genuine
rewrite and belongs to the copy-and-swap path.

### `numeric(10,2)` → `numeric(10,3)`: the value survives, the datum does not

Increasing the scale looks like pure widening — every `numeric(10,2)` value is
representable as `numeric(10,3)`. But `numeric` stores the **display scale** in each
datum's header, so equal values at different scales are different bytes:

| Row | Stored as `numeric(10,2)` | Stored as `numeric(10,3)` | Same value? | Same datum? |
| --- | --- | --- | --- | --- |
| 1 | `123.45` (dscale 2) | `123.450` (dscale 3) | yes | **no** |
| 2 | `7.10` (dscale 2) | `7.100` (dscale 3) | yes | **no** |
| 3 | `0.05` (dscale 2) | `0.050` (dscale 3) | yes | **no** |

After the change, `SELECT` returns `123.450`, not `123.45` — the stored value itself has
changed, and every row had to be re-encoded to make it so. `numeric_support` therefore
requires the scale to be *equal* before it will simplify; only precision may grow.

The reverse, `numeric(10,3) → numeric(10,2)`, is more obviously a rewrite: `123.456`
rounds to `123.46` — information is lost, and the values may fail the new precision.

### `numeric(12,2)` → `numeric(10,2)`: the rows fit, PostgreSQL cannot know

Shrinking precision at the same scale is the `numeric` twin of shortening `varchar`.
`numeric(10,2)` allows eight integer digits (10 − 2); `numeric(12,2)` allows ten.

| Row | Stored as `numeric(12,2)` | Under `numeric(10,2)` | Outcome |
| --- | --- | --- | --- |
| 1 | `12345678.90` (8 integer digits) | `12345678.90` | fits — bytes unchanged |
| 2 | `999.99` | `999.99` | fits — bytes unchanged |
| 3 | `1234567890.12` (10 integer digits) | — | `ERROR: numeric field overflow` — `A field with precision 10, scale 2 must round to an absolute value less than 10^8.` |

If row 3 does not exist, every datum is already a valid `numeric(10,2)` — and PostgreSQL
still rewrites the table, because `numeric_support` cannot prove from the typmods alone
that no such row exists, and the rewrite test never looks at data. If row 3 *does* exist,
the statement fails on reaching it, after rewriting every row before it (the whole
statement rolls back; the time and lock are spent regardless).

## No rewrite is not no cost

- **`ACCESS EXCLUSIVE` is still taken** for the duration of the statement. Milliseconds of
  work, but the lock request queues behind every in-flight query on the table and every
  later query queues behind it — the lock-queue pile-up in
  [mysql-vs-postgresql.md](mysql-vs-postgresql.md#why-ddl-is-dangerous-the-lock-queue).
  Always run under `lock_timeout`.
- **Indexes are dropped and recreated** in the same statement, even for a relabel. Their
  physical storage is *reused* only when `CheckIndexCompatible` passes: same operator
  class, same collation, and same opclass options on every key column; no expression
  columns; no predicate; index currently valid. `varchar → text` passes (a `varchar`
  index already uses `text_ops`). An expression index or a partial index on the column is
  rebuilt regardless.
- **Constraints referencing the column** are likewise dropped and re-added in the same
  statement.
- **A view or rule referencing the column blocks the statement outright** —
  `cannot alter type of a column used by a view or rule` — whether or not the change would
  have been a relabel. The view has to be dropped and recreated around the change.
- **A stored generated column whose expression references the altered column blocks the
  statement the same way** — `cannot alter type of a column used by a generated column`
  (`ERRCODE_FEATURE_NOT_SUPPORTED`), again regardless of whether the change would have been a
  relabel. `RememberAllDependentForRebuilding` refuses rather than re-plans the generation
  expression. The generated column has to be dropped before the change and re-added after
  it — and re-adding a `STORED` generated column is itself a full rewrite.

## Checking before you run it

Ask the catalog for the current type and modifier (the part the DDL text does not tell
you):

```sql
SELECT format_type(atttypid, atttypmod), attcollation::regcollation
FROM   pg_attribute
WHERE  attrelid = 't'::regclass AND attname = 'col';
```

Then test empirically inside a transaction and roll back. A table rewrite allocates a new
relfilenode; a relabel does not:

```sql
BEGIN;
SELECT pg_relation_filenode('t'::regclass);
ALTER TABLE t ALTER COLUMN col TYPE varchar(100);
SELECT pg_relation_filenode('t'::regclass);   -- unchanged ⇒ no rewrite
ROLLBACK;
```

Run this against a copy or a scratch database when the table is large: the `ALTER` inside
the transaction performs the full rewrite before you roll it back.

### Failing closed with a `table_rewrite` event trigger

PostgreSQL can refuse, database-wide, any `ALTER TABLE` (or `ALTER TYPE`) that would
rewrite a table. The `table_rewrite` event fires *before* the rewrite starts:

```sql
CREATE FUNCTION refuse_rewrite() RETURNS event_trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ALTER TABLE on % would rewrite the table',
    pg_event_trigger_table_rewrite_oid()::regclass;
END $$;

CREATE EVENT TRIGGER no_table_rewrite ON table_rewrite EXECUTE FUNCTION refuse_rewrite();
```

This is **PostgreSQL-specific**; MySQL has no equivalent hook. The two engines protect
against an accidental table copy in opposite ways:

| | PostgreSQL | MySQL 8.0 |
| --- | --- | --- |
| Mechanism | A database-wide **event trigger** on `table_rewrite` | A **per-statement assertion**: `ALTER TABLE … ALGORITHM=INSTANT` (or `ALGORITHM=INPLACE, LOCK=NONE`) |
| Who opts in | The DBA, once, for every statement anyone runs | The author of each statement |
| On violation | The trigger raises; the statement fails before any row is copied | The server refuses: `ALGORITHM=INSTANT is not supported` (or the equivalent for `INPLACE`) |
| Scope | `ALTER TABLE` and `ALTER TYPE` rewrites only — not `CLUSTER` or `VACUUM FULL` | The statement it is attached to |

Creating an event trigger requires superuser (managed services expose an equivalent, such
as `rds_superuser` on Amazon RDS and Aurora). The trigger function can be selective —
`pg_event_trigger_table_rewrite_reason()` returns why the rewrite is happening, and the
function can consult the table's size before deciding to raise.

## How pg-sprite classifies type changes

The planner's `binary-coercible` verdict reason
([plan-report.md](plan-report.md#planner-decision-reasons-decisionsreason)) is deliberately narrower than what
PostgreSQL can prove. It accepts exactly:

- the same type and modifier (a no-op relabel);
- `varchar(n) → varchar(m)` with `m ≥ n`, `varchar(n) → varchar`, and `varchar → text`;
- `numeric(p,s) → numeric(p',s)` with `p' ≥ p`, and `numeric(p,s) → numeric`.

Everything else — including changes PostgreSQL would relabel, such as `varbit` or temporal
precision widening, unconstrained domains, `xml → text`, and the UTC `timestamp ↔
timestamptz` case — is treated as `type-rewrite` and routed to the copy-and-swap path (a
typed refusal until that executor lands). The bias is intentional: a false "free" verdict
would run a blocking rewrite on a hot table; a false "rewrite" verdict costs a slower but
safe path. The rule set grows only when a new row has been proven against the reference
table in [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md).

Because the decision needs the column's current type, the classifier takes the introspected
catalog shape as input. Without it, no type change can be proven free, and the planner
routes the statement to the rewrite path rather than gamble.

## Source pointers

All in the PostgreSQL repository, `master` at time of writing; the behaviour is unchanged
across the 14–18 range.

| What | Where |
| --- | --- |
| The rewrite decision loop | `ATColumnChangeRequiresRewrite()` — `src/backend/commands/tablecmds.c` |
| Building and simplifying the transform expression | `ATPrepAlterColumnType()` — `src/backend/commands/tablecmds.c` (calls `coerce_to_target_type()` then `expression_planner()`) |
| `timestamp ↔ timestamptz` UTC exemption | `TimestampTimestampTzRequiresRewrite()` — `src/backend/utils/adt/timestamp.c` |
| Typmod no-op rules | `varchar_support()` (`varchar.c`), `numeric_support()` (`numeric.c`), `varbit_support()` (`varbit.c`), `timestamp_support()` → `TemporalSimplify()` (`timestamp.c`, `datetime.c`), `interval_support()` (`timestamp.c`) |
| Display scale stored in the `numeric` datum | `NumericShort` / `NumericLong` headers and `NUMERIC_DSCALE()` — `src/backend/utils/adt/numeric.c` |
| Index storage reuse after a no-rewrite change | `ATPostAlterTypeParse()` → `TryReuseIndex()` → `CheckIndexCompatible()` — `tablecmds.c`, `indexcmds.c` |
| View / rule and generated-column dependency refusals | `RememberAllDependentForRebuilding()` — `tablecmds.c` |
| Cast methods | `CoercionMethod` enum — `src/include/catalog/pg_cast.h` |
