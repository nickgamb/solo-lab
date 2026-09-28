#!/usr/bin/env bash
# Rewind the demos to a first run, without rebuilding anything.
#   Alice      grants, connections, ledger, pending asks AND her edited terms
#              (the owners row is the seed marker: defaults re-seed on restart), on the
#              current CNPG primary
#   Bob's side a fresh agent key (a new, unknown agent) and an empty follow-up list
. "$(dirname "$0")/lib.sh"
need_cluster
if K get cluster.postgresql.cnpg.io uma-as-db -n alice >/dev/null 2>&1; then
  step "Alice: grant state and terms"
  primary=$(K get cluster.postgresql.cnpg.io uma-as-db -n alice -o jsonpath='{.status.currentPrimary}')
  K exec -n alice "$primary" -c postgres -- psql -U postgres -d u4a -q -c \
    "TRUNCATE tickets, negotiations, rpts, connections, ledger, owner_events, tiers, terms_docs, owners;" >/dev/null
  K rollout restart deploy/uma-as -n alice >/dev/null
  K rollout restart deploy/u4a-adapter -n sv-u4a >/dev/null 2>&1 || true
  rollout alice deploy/uma-as
  ok "cleared on $primary; Alice's default terms restored; adapter has a new agent key"
fi
if K get deploy bob-workspace -n sv-mcp >/dev/null 2>&1; then
  step "Bob: workspace follow-ups"
  K rollout restart deploy/bob-workspace -n sv-mcp >/dev/null; rollout sv-mcp deploy/bob-workspace
  ok "cleared"
fi
