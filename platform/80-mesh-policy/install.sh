#!/usr/bin/env bash
# Mesh baselines for the platform-owned parties. Alice's and Meridian's are
# installed with their story (demos/bob-to-alice), because they own them.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
step "Sterling & Vance mesh baseline"
apply_tmpl "$D/sterling-vance.yaml"
ok "STRICT: sv-identity sv-agents sv-mcp; identity-scoped ALLOWs on Keycloak, kagent, agents"
