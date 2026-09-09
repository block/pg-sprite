# The machine-readable capabilities contract

The support matrix is both documentation and an API: people read it to decide whether
pg-sprite owns a schema change, while tooling needs the same answer without scraping
Markdown or interpreting prose. This document decides the source data, row shape,
generation direction, and JSON surface for that contract.

> TL;DR: **`pkg/capabilities/capabilities.yaml` is the single source of truth.** A typed
> `pkg/capabilities` package validates and embeds it, a generator replaces only marked
> tables in [capabilities.md](capabilities.md), and `pg-sprite capabilities --json`
> exposes the embedded matrix together with the binary version. CI regenerates the
> Markdown and requires an empty diff.

## Table of contents

- [The row we have to preserve](#the-row-we-have-to-preserve)
- [Decision: YAML is authoritative](#decision-yaml-is-authoritative)
- [The typed row](#the-typed-row)
- [Generated Markdown and the sync gate](#generated-markdown-and-the-sync-gate)
- [CLI JSON and query recipes](#cli-json-and-query-recipes)
- [Versioning](#versioning)
- [Shared refusal vocabulary](#shared-refusal-vocabulary)
- [Alternatives considered](#alternatives-considered)
- [Non-goals and sequence](#non-goals-and-sequence)

## The row we have to preserve

The current matrix has seven area headings and five displayed columns. The headings
become `area`; the first column is named `operation`, `table_shape`, or
`object_operation` depending on the table, but all three are the same free-form
`operation` value. The remaining displayed values are:

- `status_mark`: exactly `✅`, `🟡`, `⚪`, `🔵`, or `❌`. They mean, respectively,
  supported today, planned with a typed refusal today, out of scope because there is
  no online-safety problem, out of scope because another tool class owns the work, and
  out of scope because PostgreSQL has no online mechanism.
- `tier`: `t1`, `t2`, or `t3`, derived consistently from the mark but stored and
  validated so a consumer need not understand emoji. `✅` is T1, `🟡` is T2, and all
  three remaining marks are T3.
- `engine_path`: exactly `native_as_is`, `native_safer_sequence`,
  `native_planned_flow`, `copy_and_swap`, or `none`. These are the typed spellings of
  “native, as-is”, “native, safer sequence”, “native, planned flow”,
  “copy-and-swap”, and “—”.
- `online_safety_problem`: the leading `Yes` or `No` answer. Several rows append an
  important qualification to that answer; `online_safety_detail` preserves it rather
  than forcing consumers to parse the answer cell.
- `reason_notes`: the complete Markdown from “Behavior and why”. It remains Markdown
  because links, code spans, and safety emphasis are part of the human contract.

The prose also carries facts that are not consistently visible as their own columns:
which front door admits the operation, which tool class owns a `No` row, and the typed
verdict reason when one exists. The structured row makes those facts explicit. This is
intentional normalization, not an attempt to infer them while rendering.

## Decision: YAML is authoritative

The repository will add `pkg/capabilities/capabilities.yaml`, and that file will be the
only place where matrix rows are edited. It lives in the package that owns its typed
contract, so `go:embed` can ship that exact file without a copied artifact or generated
byte literal. Reviewers changing a public support claim still inspect the YAML and
rendered documentation in one diff.

A new `pkg/capabilities` package will define Go structs, load the YAML, validate every
closed value and cross-field invariant, expose the rows, and embed that same file with
`go:embed`. Its tests must prove that the committed file parses, every enum is known,
IDs are unique, marks agree with tiers, `No` rows name an owner, and `none` is the only
path on T3 rows.

```text
┌───────────────────────┐     ┌──────────────────────┐     ┌────────────────────────┐
│ capabilities.yaml     │────▶│ Markdown generator   │────▶│ capabilities.md tables │
└───────────┬───────────┘     └──────────┬───────────┘     └───────────┬────────────┘
            │                            │                             │
            │ go:embed                   │ regenerate                  │ committed
            ▼                            ▼                             ▼
┌───────────────────────┐     ┌─────────────────────────────────────────────────────┐
│ pkg/capabilities      │     │ CI gate: regenerate, then require an empty git diff │
└───────────┬───────────┘     └─────────────────────────────────────────────────────┘
            │
            ▼
┌────────────────────────────────────────┐
│ pg-sprite capabilities --json          │
└────────────────────────────────────────┘
```

## The typed row

YAML uses stable snake-case values; Go gives each closed vocabulary a named string
type and rejects unknown values at load time. JSON uses the same field names and enum
spellings.

| Field | Type | Required | Contract |
| --- | --- | --- | --- |
| `id` | string | yes | Stable, unique, lowercase kebab-case identity; independent of display wording |
| `area` | enum | yes | `column_changes`, `constraints`, `indexes`, `partitioned_tables`, `declarative_model`, `types_and_non_table_objects`, `data_and_whole_table_operations` |
| `operation` | string | yes | Lossless Markdown label from the first displayed column |
| `tier` | enum | yes | `t1`, `t2`, `t3` |
| `status_mark` | enum | yes | `✅`, `🟡`, `⚪`, `🔵`, `❌` |
| `engine_path` | enum | yes | `native_as_is`, `native_safer_sequence`, `native_planned_flow`, `copy_and_swap`, `none` |
| `online_safety_problem` | boolean | yes | The matrix's leading Yes/No answer |
| `online_safety_detail` | string | no | Markdown qualification after Yes/No, without the leading answer |
| `owning_tool_class` | string | conditionally | Required when `online_safety_problem` is false; absent otherwise; preserves the class named after “No —” |
| `front_doors` | object | yes | Exactly `migrate` and `diff`, each `supported`, `refused`, or `not_applicable` |
| `refusal_reason` | enum | no | Existing `verdict.Reason` token when the row has one; for example `backend-unavailable` or `unsupported-statement` |
| `reason_notes` | string | yes | Lossless Markdown from “Behavior and why” |

`refusal_reason` reuses the exact kebab-case values returned by
`verdict.Reasons()` in `pkg/verdict/verdict.go`; it does not invent parallel names.
The helpers in `pkg/migrate/verdicts.go` remain the authority for which reason runtime
execution emits. A row without one stable runtime reason omits the field rather than
claiming more precision than the engine provides. `front_doors` records admission,
not whether a direct SQL equivalent exists outside pg-sprite. `not_applicable` is for
an operation that a door cannot express; `refused` means that door can identify and
reject it.

The renderer reconstructs the existing answer cell from
`online_safety_problem`, `online_safety_detail`, and `owning_tool_class`. It renders
the human engine-path labels from the enum and uses `reason_notes` verbatim. This
preserves every current cell, including long qualifications and Markdown links.

One real row will produce this JSON object (the surrounding response is described
below):

```json
{
  "id": "refresh-materialized-view",
  "area": "types_and_non_table_objects",
  "operation": "`REFRESH MATERIALIZED VIEW`",
  "tier": "t3",
  "status_mark": "🔵",
  "engine_path": "none",
  "online_safety_problem": false,
  "owning_tool_class": "data jobs / owner tooling",
  "front_doors": {
    "migrate": "refused",
    "diff": "not_applicable"
  },
  "refusal_reason": "unsupported-statement",
  "reason_notes": "A data operation, not catalog work: the plain form holds `ACCESS EXCLUSIVE` on the matview for the whole rebuild (`CONCURRENTLY` needs a unique index and trades the lock for churn). Scheduling refreshes belongs to data jobs"
}
```

## Generated Markdown and the sync gate

The generator replaces only content between paired markers such as
`<!-- capabilities:begin column_changes -->` and
`<!-- capabilities:end column_changes -->`. One more pair,
`<!-- capabilities:begin summary -->` and `<!-- capabilities:end summary -->`, wraps the
headline sentence above the tables that counts operations per tier: those counts are
derived from the rows, so the generator writes them, and a row added to the YAML cannot
leave the sentence one behind. The status legend below it stays hand-written; the mark set
is a closed enum and the legend explains it rather than counting it. Everything outside the
markers — the introduction, tier explanation, legend, peer comparison, refusal rationale,
and operator recipes — remains hand-written. Generated output is deterministic: source
order is display order, formatting has no timestamps, and a second generation is a no-op.

The generator lands with the YAML file, not later. A Make target runs its `go run`
entry point. CI runs that target and then fails unless `git diff --exit-code` is empty.
The test validates semantics; regenerate-and-diff proves the checked-in human page is
the rendering of the validated data.

The capability-statement rule still applies beyond the generated matrix. A behavior
change updates the YAML, [limitations.md](limitations.md), and the README's short
limitations section in one change. Generation cannot safely rewrite those narrative
summaries: they explain refusal mechanics and select highlights rather than duplicate
one row per capability. Their existing review rule remains the appropriate sync gate.

## CLI JSON and query recipes

`pg-sprite capabilities --json` prints one object with `version` and `capabilities`.
Array order is source order and therefore stable for display, but consumers should
select by fields or `id`, not array position. The command reads only embedded data and
does not connect to PostgreSQL.

The contract must support these queries:

```sh
# All T2 rows.
pg-sprite capabilities --json | jq '.capabilities[] | select(.tier == "t2")'

# Everything the declarative door refuses.
pg-sprite capabilities --json | jq '.capabilities[] | select(.front_doors.diff == "refused")'

# Rows owned by another tool class.
pg-sprite capabilities --json | jq '.capabilities[] | select(.owning_tool_class != null)'
```

Human output may render a compact table, but JSON field names and enum values are the
automation contract. Stable JSON means deterministic content and closed vocabulary;
it does not mean preserving insignificant whitespace.

## Versioning

The YAML has no separate matrix version. It is committed with, embedded in, and tested
against one pg-sprite binary, so a second version could disagree with the artifact that
actually answers the question. The command's top-level `version` is the binary version
string. Consumers that need a fixed matrix pin the binary version (and can archive its
JSON); development builds report the same version convention as the rest of the CLI.

Adding an enum value or field is an additive contract change. Removing or changing a
meaning waits for a normal pg-sprite compatibility boundary. Consumers must ignore
unknown fields and fail closed or report “unknown” for enum values they do not
understand.

## Shared refusal vocabulary

A refusal `class` on the verdict JSON is a separate decision, recorded in
[refusal-classes.md](refusal-classes.md); `pkg/verdict` does not emit one today. When that
field ships, that contract owns the vocabulary and the matrix uses the same words, so a
consumer reading a row and a consumer reading a verdict reach the same route:

| Matrix row | Refusal `class` |
| --- | --- |
| T2 / `🟡` | `capability-boundary` |
| T3 / `⚪` | `no-online-safety-problem`, with `owner: direct-operator` |
| T3 / `🔵` | `no-online-safety-problem`, with `owner` naming the tool class |
| T3 / `❌` | `by-design` |

T1 / `✅` has no capability refusal class. Two classes have no matrix row: `environmental`
describes the run site — privileges, contention, budgets, or other conditions — not whether
pg-sprite supports an operation, and `invariant-violation` reports a defect in pg-sprite,
not a property of the operation. The matrix and that contract must change together.

## Alternatives considered

### Hand-maintain YAML and Markdown, then compare

This preserves Markdown as the pleasant row-authoring format and makes prose-heavy
changes easy to review. It loses because two editable representations inevitably
drift, and an equivalence checker must either parse Markdown (including links, code
spans, and long cells) or quietly compare less than the full contract. Generated
Markdown is still reviewed in the same diff, while markers preserve the prose that
benefits from direct Markdown editing.

### JSON as source

JSON is directly embeddable and needs no YAML decoder, but multiline Markdown strings,
comments, and trailing-comma-free edits make the source unnecessarily hostile to
documentation review. JSON remains the CLI output because its consumers are tools;
YAML is the authoring format because its consumers are maintainers.

### Go literals as source

Go literals provide compile-time field names and enum constants, need no parser, and
embed naturally in the binary. They lose on the other lens: changing public support
documentation would require editing quoted Go strings, and non-Go contributors would
have to understand package syntax. Typed loading plus validation supplies the safety
benefit without making implementation code the documentation source.

### Put YAML at the repository root or under `docs/`

The root makes the contract prominent but adds another top-level project artifact and
separates it from both owners. `docs/` puts the source beside the page it renders, but
Go's embed patterns cannot reach outside the embedding package; shipping that file
would require a copied artifact or generated Go bytes. Keeping the single source under
`pkg/capabilities` wins because the package owns the schema, validation, and embedded
artifact. The generated `docs/capabilities.md` remains the human-facing home.

## Non-goals and sequence

This decision does not build sortable HTML tables or a documentation site. It also
does not change any capability, tier, refusal, or runtime behavior.

Implementation order is:

1. add the typed package, `pkg/capabilities/capabilities.yaml`, validator, generator,
   and markers together, making the repository single-source on day one;
2. add `pg-sprite capabilities`, including `--json` and the embedded binary version;
3. add the regenerate-and-diff CI gate to the normal pipeline; and
4. add documentation and `jq` recipes for consumers.

The generator is part of the first step rather than a cleanup step: there is never an
intermediate state in which two hand-maintained matrices are authoritative.
