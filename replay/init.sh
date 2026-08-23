#!/usr/bin/env bash
#
# Scaffold a new corpus replay project from the three inputs the pattern
# needs: the public repository, a commit (or ref, resolved to a commit), and
# the path of the schema-change files inside it. Writes
# replay/<project>/{project.conf,.gitignore} with the corpus file list
# captured at the pin, plus a skeleton assessment.tsv to curate.
#
#   ./init.sh <project> <owner/repo|github-url> <commit-or-ref> <migrations-path>
#   ./init.sh chuzz https://github.com/example/chuzz main migrations
set -euo pipefail

REPLAY_DIR="$(cd "$(dirname "$0")" && pwd)"
. "${REPLAY_DIR}/common.sh"

[ $# -eq 4 ] || die "usage: $0 <project> <owner/repo|github-url> <commit-or-ref> <migrations-path>"
project="$(basename "$1")"
repo="$2"
ref="$3"
migrations_path="${4%/}"

project_dir="${REPLAY_DIR}/${project}"
[ -e "${project_dir}/project.conf" ] && die "replay/${project} already exists"

slug="${repo#https://github.com/}"
slug="${slug#http://github.com/}"
slug="${slug%.git}"
slug="${slug%/}"
case "$slug" in
    */*) ;;
    *) die "repository must be owner/name or a github.com URL, got: ${repo}" ;;
esac

api="https://api.github.com/repos/${slug}"
sha="$(curl -fsSL "${api}/commits/${ref}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha"])')"
files="$(curl -fsSL "${api}/contents/${migrations_path}?ref=${sha}" \
    | python3 -c 'import json,sys; print("\n".join(sorted(e["name"] for e in json.load(sys.stdin) if e["name"].endswith(".sql"))))')"
[ -n "$files" ] || die "no .sql files at ${slug}@${sha:0:12}:${migrations_path}"
baseline="$(printf '%s\n' "$files" | head -1)"

# Each project gets its own host port so harnesses can coexist; pick one past
# the highest port any existing project claims.
port="$(awk -F= '/^PORT=/ {gsub(/[^0-9]/,"",$2); if ($2+0 > max) max=$2+0} END {print (max ? max+1 : 5439)}' \
    "${REPLAY_DIR}"/*/project.conf 2>/dev/null || echo 5439)"

mkdir -p "$project_dir"

{
    echo "# Replay project: ${project} — see replay/README.md for the pattern."
    echo "# The corpus is pinned to one commit so every replay run assesses the"
    echo "# same history; bumping COMMIT means re-curating assessment.tsv."
    echo "REPO=\"${slug}\""
    echo "COMMIT=\"${sha}\""
    echo "MIGRATIONS_PATH=\"${migrations_path}\""
    echo "PG_IMAGE=\"postgres:17-alpine\""
    echo "PORT=${port}"
    echo ""
    echo "# Applied via psql as the starting state (bootstrap DDL on an empty"
    echo "# database has no online-safety problem); empty means start empty."
    echo "BASELINE=\"${baseline}\""
    echo ""
    echo "# The complete corpus at the pinned commit. An explicit list, not a"
    echo "# directory scrape: a fetch that silently picked up new files would"
    echo "# desynchronize the corpus from the assessment written against it."
    echo "FILES=("
    printf '%s\n' "$files" | sed 's/^/    /'
    echo ")"
} >"${project_dir}/project.conf"

echo "corpus/" >"${project_dir}/.gitignore"

{
    echo "# Replay assessment for ${project} — curated against ${slug}@${sha:0:12}."
    echo "# Line ranges index into corpus/<file>.sql at exactly that pin."
    echo "#"
    echo "# One row per replay step, in strict corpus order:"
    echo "#   <migration-prefix> <start>-<end> <execute|refuse:<reason>|psql>"
    echo "#"
    echo "# expected:"
    echo "#   execute          require exit 0 and outcome executed-natively —"
    echo "#                    pg-sprite itself mutates the database"
    echo "#   refuse:<reason>  require exit 2 and exactly <reason>, then apply"
    echo "#                    the same statement via psql to advance"
    echo "#   psql             out-of-scope content: applied via psql, never assessed"
    echo "#"
    echo "# TODO: curate the history after the baseline (${baseline}) here."
} >"${project_dir}/assessment.tsv"

echo "scaffolded replay/${project} at ${slug}@${sha:0:12} (port ${port})"
echo
echo "next steps:"
echo "  1. ./fetch.sh ${project}          # download the pinned corpus"
echo "  2. ./harness.sh ${project} up     # start postgres, apply the baseline"
echo "  3. curate ${project}/assessment.tsv (probe verdicts with:"
echo "     bin/pg-sprite migrate --url \"\$(./harness.sh ${project} dsn)\" --json --alter '...')"
echo "  4. ./replay.sh ${project}         # replay and iterate until green"
