# Export a declarative baseline with `pull`

`pg-sprite pull` reads every ordinary, non-partition-child table in one live
schema and creates a desired-state SQL file for each table. Use it to put an
existing database under declarative management, then use `diff` to prove that
the exported files describe the same schema.

```sh
pg-sprite pull --url "$PG_DSN" --schema public --out schema
```

`--url` accepts a PostgreSQL URL or key/value DSN and can instead be supplied
as `PGSPRITE_URL`. `--schema` defaults to `public`; `--out` (short form `-o`)
defaults to `schema`. The shared database flags also provide TLS, timeout, and
debug controls; run `pg-sprite pull --help` for the complete list.

## Output and exit status

For a schema containing `accounts` and `events`, the output directory is:

```text
schema/
├── accounts.sql
└── events.sql
```

Each file contains one canonical `CREATE TABLE`, followed by that table's
`CREATE INDEX` statements. Export is create-only: `pull` creates the output
directory when necessary but never overwrites a file. Move or delete an old
baseline before refreshing it.

Tables are processed independently rather than fail-fast. The text report has
one `PULLED`, `REFUSED`, or `ERROR` result per table and a final count. A fully
successful export exits 0; one or more model refusals exit 2; an operational
error (including an existing output file, unsafe or case-colliding file names,
or a missing schema) exits 1. If both refusals and operational errors occur,
the operational error takes precedence. `pull` does not currently have JSON
output.

## Baseline export and zero-diff verification

Start with an empty destination, export the live schema, and verify every file
against the same database before committing the baseline:

```sh
export PGSPRITE_URL='postgres://user:password@localhost/database?sslmode=disable'

rm -rf schema
pg-sprite pull --schema public --out schema

for desired in schema/*.sql; do
  pg-sprite diff --schema public --desired "$desired" --json |
    jq -e '.disposition == "execute" and (.statements | length == 0)' >/dev/null
done

git add schema
git commit -m 'Add declarative schema baseline'
```

The loop exits non-zero if any exported table produces a schema change plan.
Run it against the same database and schema used by `pull`; each desired file
is single-table scoped, while `--schema` tells `diff` where to find that live
table.

This is the command-level form of `schemadiff.Render`'s round-trip guarantee:
**introspect → render → parse → diff = zero changes**. `pull` calls
`schemadiff.Introspect` and `schemadiff.Render` for each table; `Render` parses
its own output as a desired file, and integration tests materialize that output
and prove that its diff from the source model is empty.

## Refused table shapes

Export fails closed when the declarative model cannot represent a table
without losing meaning. Current refusals include:

- partitioned parents (partition children are not independently exported);
- either side of classic `INHERITS` relationships;
- either side of a foreign-key relationship;
- unlogged tables and columns with explicit collations; and
- sequence-backed defaults that cannot be rendered as an owned `serial` form.

Extension-owned tables are excluded from enumeration. Comments, storage
parameters, and non-table objects are outside the declarative model and are not
exported; manage them separately. See the complete
[declarative model boundaries](limitations.md#declarative-model-boundaries)
and [support matrix](capabilities.md#the-declarative-model-desired-files-diff-pull).
