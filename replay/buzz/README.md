# Buzz corpus replay

The first [corpus replay](../README.md) project: the SQLx migration history of
[block/buzz](https://github.com/block/buzz), a Rust service whose relay database
exercises range partitioning, composite foreign keys, enum types, generated TSVECTOR
columns, GIN and partial indexes, and PL/pgSQL triggers — a demanding, honest sample
of what production PostgreSQL schemas look like.

The corpus files are buzz's SQLx migrations (their terminology, cited as-is);
pg-sprite assesses only the schema-change content inside them. The baseline is corpus
file `0001_initial_schema.sql`, and [assessment.tsv](assessment.tsv) curates the
history after it. See [replay/README.md](../README.md) for usage — buzz is the
default project, so from the repository root:

```sh
make replay
```

Each refusal row pins the class emitted by pg-sprite; the engine's classification is
authoritative. See the [refusal class contract](../../docs/refusal-classes.md).

## Boundary facts the curation surfaced

Curating the manifest against real verdicts (not predictions) established three
behaviors worth knowing when reading the results:

- `CREATE INDEX IF NOT EXISTS` is refused by design — a name-only no-op cannot prove
  the existing index is valid or even the requested one.
- Buzz's unnamed `ADD FOREIGN KEY` executes via the safer `NOT VALID` + `VALIDATE`
  sequence: the `VALIDATE` step has to name the constraint, so pg-sprite states the
  name PostgreSQL would choose (`<table>_<columns>_fkey`).
- Every `DROP COLUMN`, `DROP CONSTRAINT`, and `DROP NOT NULL` is refused by design
  (`destructive-change`): the replay never passes `--allow-destructive`, so it pins
  the refusal and advances the history through psql.
