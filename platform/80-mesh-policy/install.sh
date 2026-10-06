#!/usr/bin/env bash
# Mesh baselines for the platform-owned parties. Alice's and Meridian's are
# installed with their story (demos/bob-to-alice), because they own them.
# Layer 45 applies S&V's first, before any S&V workload; this re-applies it.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
step "Sterling & Vance mesh baseline"
apply_tmpl "$D/sterling-vance.yaml"
ok "STRICT: sv-identity sv-workforce sv-agents sv-egress kagent agentgateway-system; identity-scoped ALLOWs on Keycloak, kagent, agents, egress"

step "Sterling & Vance egress: out only through the firm's gateways"
deny_internet sv-identity sv-workforce kagent sv-agents sv-mcp agentregistry
ok "no direct internet from sv-identity sv-workforce kagent sv-agents sv-mcp agentregistry (ai-gateway and sv-egress are the ways out)"
