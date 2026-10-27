# Execute and introspect: why `diff` asks PostgreSQL

`pg-sprite diff` compares a desired `CREATE TABLE` with the live table. To do that it runs the
desired DDL on the target server, inside a transaction it always rolls back, and reads back what
PostgreSQL built. [architecture.md](architecture.md#why-introspect--ask-postgresql-dont-reimplement-it)
gives the reason in one paragraph and walks through the seven steps. This page fills in the
detail: what PostgreSQL rewrites when it stores a table, shown on a real example, the spellings
it does not fold, and what the approach requires from the database.

## Table of contents

- [The problem](#the-problem)
- [A worked example](#a-worked-example)
- [How pg-sprite avoids reimplementing the rules](#how-pg-sprite-avoids-reimplementing-the-rules)
- [What the catalog does not fold](#what-the-catalog-does-not-fold)
- [What the approach requires](#what-the-approach-requires)

## The problem

PostgreSQL does not store DDL as text. It parses the statement, resolves names and types, fills
in defaults it chose itself, and keeps the result in its catalogs. When you ask for the
definition back, the server prints it from those catalogs. So the text a person writes never
matches the text the server reports, even when the two describe exactly the same table.

A declarative diff has to compare like with like. If it compared the file's text with the
server's text, every table would show changes on every run: `VARCHAR(255)` against
`character varying(255)`, an unnamed `UNIQUE` against `users_email_key`, and so on. Each of
those false changes would turn into a real `ALTER TABLE` against a production table.

## A worked example

The desired table, as a person writes it:

```sql
CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    active BOOL DEFAULT TRUE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    score INT CHECK (score >= 0 AND score <= 100),
    note TEXT DEFAULT 'none'
);
CREATE INDEX ON users (lower(email));
```

What PostgreSQL 16 reports for it, read with `format_type`, `attnotnull`, `pg_get_expr`,
`pg_get_constraintdef`, and `pg_get_indexdef`:

```text
-- columns: name | type | not null | default
id         | integer                     | t | nextval('users_id_seq'::regclass)
email      | character varying(255)      | t |
active     | boolean                     | f | true
created_at | timestamp without time zone | f | CURRENT_TIMESTAMP
score      | integer                     | f |
note       | text                        | f | 'none'::text

-- constraints
users_email_key   | UNIQUE (email)
users_pkey        | PRIMARY KEY (id)
users_score_check | CHECK (((score >= 0) AND (score <= 100)))

-- indexes
users_email_key | CREATE UNIQUE INDEX users_email_key ON public.users USING btree (email)
users_lower_idx | CREATE INDEX users_lower_idx ON public.users USING btree (lower((email)::text))
users_pkey      | CREATE UNIQUE INDEX users_pkey ON public.users USING btree (id)

-- sequences
users_id_seq | owned by column id of table users
```

To read the same on your own server, for any table:

```sql
SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull, pg_get_expr(d.adbin, d.adrelid)
FROM pg_attribute a
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = 'users'::regclass AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum;

SELECT conname, pg_get_constraintdef(oid)
FROM pg_constraint
WHERE conrelid = 'users'::regclass
ORDER BY conname;

SELECT indexrelid::regclass, pg_get_indexdef(indexrelid)
FROM pg_index
WHERE indrelid = 'users'::regclass
ORDER BY indexrelid::regclass::text;
```

Every line differs from what was written:

| Written | Stored |
| --- | --- |
| `id SERIAL` | `integer NOT NULL`, a new sequence `users_id_seq` owned by the column, and the default `nextval('users_id_seq'::regclass)` |
| `VARCHAR(255)`, `BOOL`, `TIMESTAMP`, `INT`, `TEXT` | `character varying(255)`, `boolean`, `timestamp without time zone`, `integer`, `text` |
| `PRIMARY KEY` and `UNIQUE` on the column | table constraints named `users_pkey` and `users_email_key`, each with an index of the same name |
| `CHECK (score >= 0 AND score <= 100)` | `CHECK (((score >= 0) AND (score <= 100)))`, fully parenthesized and named `users_score_check` |
| `DEFAULT TRUE` | `true` |
| `DEFAULT 'none'` | `'none'::text`, with an explicit cast |
| `CREATE INDEX ON users (lower(email))` | an index named `users_lower_idx`, with `email` cast to `text` inside `lower()` |

An engine that applied the DDL to an in-memory model instead would have to reproduce every one
of these rules exactly — the alias table, the naming scheme and its collision handling, where
casts and parentheses appear — for every major version it supports. The rules do shift. On
PostgreSQL 14 and 16 the same table has the three constraints above; on PostgreSQL 18 it has
five, because 18 also records each `NOT NULL` as a named constraint:

```text
-- PostgreSQL 18: conname | contype | pg_get_constraintdef
users_email_key      | u | UNIQUE (email)
users_email_not_null | n | NOT NULL email
users_id_not_null    | n | NOT NULL id
users_pkey           | p | PRIMARY KEY (id)
users_score_check    | c | CHECK (((score >= 0) AND (score <= 100)))
```

Everything else in the output above is identical on 14, 16, and 18 for this table.

The model `diff` compares is built from this output with two adjustments, both applied the same
way to the live table and to the desired one. Indexes that back a primary key, unique, or
exclusion constraint are represented by their constraint, not listed again as indexes. And the
schema that `pg_get_indexdef` always prints in the `ON` clause is removed, so the model does not
depend on which schema the table was built in: `users_lower_idx` is compared as
`CREATE INDEX users_lower_idx ON users USING btree (lower(email::text))`. The PostgreSQL 18
`NOT NULL` constraints are not read; `NOT NULL` is taken from the column on every major, so the
model is the same across versions.

## How pg-sprite avoids reimplementing the rules

pg-sprite builds the desired table on the target server, in a schema created inside a
transaction, and reads it with the same introspection code it uses for the live table. Then it
rolls the transaction back. Both sides of the comparison have passed through PostgreSQL's own
rewriting and are read the same way, so the diff is a plain comparison of two models. It
contains no rules about which spellings are equivalent. The step-by-step flow is in
[architecture.md](architecture.md#why-introspect--ask-postgresql-dont-reimplement-it).

Copy-and-swap uses the same mechanism. The schema a shadow table should have after the change,
which a resumed copy checks before it continues, comes from the same rolled-back scratch
schema, not from editing the DDL in Go
([copy-and-swap D1](copy-and-swap-design.md#d1--no-durable-scratch-database)).

Engines that diff offline take the other route. Spirit, for MySQL, runs a
[normalization layer](https://github.com/block/spirit/blob/main/pkg/statement/README.md#normalization)
over parsed `CREATE TABLE` statements that approximates what MySQL does when it stores a table,
so it can diff without a server. pg-sprite carries no such layer and relies on the round trip.

## What the catalog does not fold

Some spellings mean the same thing to a person but are stored differently by PostgreSQL. The
server keeps them distinct, so `diff` reports them. Given this live table:

```sql
CREATE TABLE accounts (id bigint PRIMARY KEY, email text UNIQUE, name text,
    created_at timestamptz DEFAULT CURRENT_TIMESTAMP, credits int DEFAULT 2);
```

and this desired file:

```sql
CREATE TABLE accounts (
    id bigint PRIMARY KEY,
    email text CONSTRAINT accounts_email_uniq UNIQUE,
    name text,
    created_at timestamptz DEFAULT now(),
    credits int DEFAULT 1 + 1
);
```

`pg-sprite diff --sql` reports, identically on PostgreSQL 14, 16, and 18:

```sql
-- native (metadata-only)
-- destructive
ALTER TABLE public.accounts DROP CONSTRAINT accounts_email_key;
-- native (metadata-only)
ALTER TABLE public.accounts ALTER COLUMN created_at SET DEFAULT now();
-- native (metadata-only)
ALTER TABLE public.accounts ALTER COLUMN credits SET DEFAULT 1 + 1;
-- native (safer-idiom)
-- safer form the engine would run (not equivalent; each step in its own transaction — see https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md):
--   CREATE UNIQUE INDEX CONCURRENTLY "accounts_email_uniq" ON "public"."accounts" ("email");
--   ALTER TABLE "public"."accounts" ADD CONSTRAINT "accounts_email_uniq" UNIQUE USING INDEX "accounts_email_uniq";
ALTER TABLE public.accounts ADD CONSTRAINT accounts_email_uniq UNIQUE (email);
```

| Desired vs live | What the server stores | What `diff` reports | After one apply |
| --- | --- | --- | --- |
| `DEFAULT now()` vs `DEFAULT CURRENT_TIMESTAMP` | `now()` vs `CURRENT_TIMESTAMP` | a `SET DEFAULT` | converges: the next `diff` is empty |
| `DEFAULT 1 + 1` vs `DEFAULT 2` | `(1 + 1)` vs `2`; the server does not evaluate the expression | a `SET DEFAULT` | converges |
| a named `CONSTRAINT accounts_email_uniq UNIQUE` vs an unnamed `UNIQUE` | `accounts_email_uniq` vs `accounts_email_key` | a destructive `DROP CONSTRAINT` of the old name and an `ADD CONSTRAINT` under the new one; constraints are matched by name, so a rename is a drop and an add | `migrate --desired` refuses the whole plan with reason `destructive-change`; running the statements with `migrate --alter` converges |
| `name text COLLATE "C"` vs `name text` | collation `"C"` vs the type's default | refused: `unsupported schema change: column "name" collation change ("" vs "pg_catalog.\"C\"")` | does not converge through pg-sprite; a collation change rewrites the column and rebuilds its indexes, which the diff does not derive |

The same table written with `int8`, `TEXT`, `TIMESTAMP WITH TIME ZONE`, `current_timestamp`,
`DEFAULT (2)`, table-level `PRIMARY KEY (id)` and `UNIQUE (email)`, and `COLLATE "default"`
reports no changes against the live table: those differences the catalog does fold.

When a reported difference was not meant as a change, write the desired file the way the server
stores it. [`pg-sprite pull`](pull.md) exports a live table in that form.

## What the approach requires

- **A live connection to a server of the target's major version.** `diff` and
  `migrate --desired` (with or without `--dry-run`) need one. `lint`, `suggest`, and `fmt` read
  SQL text only and run offline ([architecture.md](architecture.md#the-five-front-end-stages)).
- **`CREATE` on the database.** The scratch schema is created with `CREATE SCHEMA`, which needs
  the database-level `CREATE` privilege. Without it the server refuses with
  `permission denied for database`. `CREATEDB` is not needed, and no other database is created
  ([engine-role.md](engine-role.md)).
- **A server that accepts writes.** The approach executes DDL, so it cannot run on a hot
  standby or read replica, which runs every transaction read-only. On a read-only session,
  `diff` fails before planning anything:

  ```text
  pg-sprite: error: create scratch schema: ERROR: cannot execute CREATE SCHEMA in a read-only transaction (SQLSTATE 25006)
  ```

  pg-sprite itself writes only the scratch schema, never the live table.
- **Bounded waits.** The session runs under `lock_timeout` and `statement_timeout` (`diff`
  defaults to 3 s and 30 s; `--lock-timeout` and `--statement-timeout` change them). Building the
  scratch table does not lock the live table it is compared with. When the desired file has
  foreign keys, each referenced table is copied empty into the scratch schema, which takes the
  `ACCESS SHARE` lock a plain `SELECT` takes on it.
- **Transactional effects do not persist.** The transaction is always rolled back, on success and
  on failure. Everything PostgreSQL ties to the transaction goes with it, including rows an
  existing event trigger writes when it fires on the scratch DDL, and a `NOTIFY` it sends, which
  PostgreSQL delivers only when the transaction commits. Nontransactional effects of such a
  trigger are not undone: `nextval()` on an existing sequence still advances it, and a write over
  `dblink` or to a file still lands. pg-sprite runs no such code itself; check the database's
  event triggers (`\dy` in psql) if they matter.
- **A result that holds for the server it ran on.** The model is whatever that server's
  decompiler printed, with that database's defaults (collation, extensions, and functions on the
  `search_path`). A diff computed against one server is not a promise about another server or
  another major version.

MySQL cannot roll back DDL, so a tool that wants the same round trip there has to create a real
temporary database and drop it afterwards. PostgreSQL's transactional DDL is what lets pg-sprite
do it inside one rolled-back transaction on the target itself.
