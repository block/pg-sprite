#!/usr/bin/env bash
#
# Fetch the buzz schema-change corpus: the SQLx migration files from the
# public https://github.com/block/buzz repository, pinned to one commit so
# every replay run assesses the same corpus. Files land in corpus/ (ignored
# by git — the corpus is buzz's, not this repository's; re-fetch instead of
# vendoring).
set -euo pipefail

cd "$(dirname "$0")"

# Pinned block/buzz commit the corpus is fetched at. Bumping this pin is a
# deliberate act: re-run the replay and reconcile any new files afterwards.
BUZZ_COMMIT="a2d8be5efa126221c7676f7797555dfb2bf5b0e0"
BASE_URL="https://raw.githubusercontent.com/block/buzz/${BUZZ_COMMIT}/migrations"

# The complete relay-database corpus at the pinned commit. An explicit list,
# not a directory scrape: a fetch that silently picked up new files would
# desynchronize the corpus from the assessment that was written against it.
FILES=(
    0001_initial_schema.sql
    0002_git_repo_names.sql
    0003_community_icon.sql
    0004_events_tags_gin.sql
    0005_agent_turn_metric_fts.sql
    0006_moderation.sql
    0007_nip_rs_retention.sql
    0008_fresh_install_search_allowlist.sql
    0009_nip_rs_database_guards.sql
    0010_nip_rs_exact_replay_guard.sql
    0011_nip_rs_exact_tag_cardinality.sql
    0012_push_leases.sql
    0013_push_endpoint_state.sql
    0014_push_lease_fts.sql
    0015_push_gateway_authority.sql
    0016_community_archival.sql
    0017_product_feedback.sql
    0018_push_match_queue.sql
    0019_mesh_status_retention.sql
    0020_join_policy_acceptances.sql
    0021_created_at_fence_floor.sql
    0022_event_ttl_refresh.sql
    0023_push_match_gate.sql
    0024_event_ttl_refresh_shared_lock.sql
    0025_relay_invites.sql
    0026_replica_heartbeat.sql
    0027_channels_id_lookup_index.sql
    0028_long_reaction_payloads.sql
    0029_community_deletion.sql
    0030_community_deletion_recovery.sql
    0031_workflow_run_error_codes.sql
    0032_channel_roster_snapshot_fence.sql
)

mkdir -p corpus

for f in "${FILES[@]}"; do
    if [ -s "corpus/$f" ]; then
        echo "have  $f"
        continue
    fi
    echo "fetch $f"
    curl -fsSL --retry 3 -o "corpus/$f.tmp" "${BASE_URL}/${f}"
    mv "corpus/$f.tmp" "corpus/$f"
done

echo
echo "corpus complete: ${#FILES[@]} files at block/buzz@${BUZZ_COMMIT:0:12}"
