# Demo tour

A runnable tour of the pg-sprite CLI against a real local PostgreSQL — and,
in check mode, the packaged-binary smoke test CI runs.

```sh
make demo    # build + compose DB up + reseed + run the whole tour
```

The tour walks four sections, each runnable on its own via
`demo/tour.sh <section>` (with `PGS` and `PG_DSN` set — see the Makefile):

| Section   | What it shows                                                                                                       | Writes?             |
| --------- | ------------------------------------------------------------------------------------------------------------------- | ------------------- |
| `dryrun`  | One statement per planner route, reason, and disposition: metadata-only, fast-default, binary-coercible, the safer idioms, a rewrite-required suggestion, type-rewrite, volatile-default, app-breaking-rename, destructive, relocation, and a refusal | no                  |
| `diff`    | The declarative front door: a routed convergence plan for an existing table and for a missing one                    | no                  |
| `offline` | `lint` (gates on error findings), `suggest` (advises), `fmt` (canonicalizes) — no database                           | no                  |
| `exec`    | Real executions: a native add, the concurrent index substitution, the four-step `SET NOT NULL` sequence, and a structured refusal (exit code 2) for a rewrite whose backend is not yet available | yes (seeded tables) |

`make demo` reseeds [seed.sql](seed.sql) first, so every run starts from the
same state and the tour is rerunnable. The compose database is left running
afterwards (`make db-down` stops it). Individual sections assume that
freshly seeded baseline: after an `exec` pass has changed the tables, run
`make demo-seed` before invoking a section directly again.

## Check mode (CI)

```sh
make demo-check   # same tour, asserting on --json fields and exit codes; needs jq
```

`CHECK=1` makes the tour assert on the typed JSON contract — plan report
routes/reasons/destructive flags and `format_version`, verdict outcomes and
reasons, the substituted `executed_sql` shape (step count plus a
distinguishing fragment, so a regression that drops `CONCURRENTLY` or
collapses the `SET NOT NULL` sequence turns the job red), statement counts —
and on exit codes (`0` success, `2` refusal, `1` lint gate). It never
asserts on human-facing prose, which is free to change. CI runs
this as the `demo` job ("smoke test (built pg-sprite artifact)"): the
built `bin/pg-sprite` exercised end-to-end against compose PostgreSQL.

This covers a gap the Go fixtures don't: they test the code, not the
artifact. The smoke test is the one check that runs the actual shipped
binary — flag parsing, Kong wiring, exit-code mapping, JSON encoding on
real stdout — the way a user invokes it.

## Keeping the tour honest

The expectation rows in [tour.sh](tour.sh) are a contract with the planner.
When a change adds, renames, or removes a planner route, reason, verdict
outcome, or safer idiom — or changes what the demo statements route to —
extend or update the rows in the same PR. See the Demo tour section of
[AGENTS.md](../AGENTS.md).
