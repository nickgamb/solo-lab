#!/usr/bin/env bash
# Story 1 (Bob): Bob's agent, his workspace (kmcp) behind an agentgateway
# waypoint with RFC 8693 delegation, and Ledgerline Research via Cross App
# Access (ID-JAG). Needs the platform (make platform). Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster

step "Images (local registry)"
build() { docker image inspect "$1" >/dev/null 2>&1 || docker build -q -t "$1" "$2" >/dev/null; docker push -q "$1" >/dev/null; ok "$1"; }
build "localhost:$LAB_REGISTRY_PORT/sv/bob-workspace:dev" "$D/mcp/bob-workspace"
build "localhost:$LAB_REGISTRY_PORT/ledgerline/research-mcp:dev" "$D/ledgerline/mcp"
build "localhost:$LAB_REGISTRY_PORT/lab/toolbox:1" "$LAB_ROOT/tools/toolbox"

step "Ledgerline Research (its own IdP, MCP server, Istio waypoint)"
K create secret generic kc-secrets -n ledgerline-identity \
  --from-literal=KC_BOOTSTRAP_ADMIN_USERNAME=admin --from-literal=KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret LL_KC_ADMIN_PASSWORD)" \
  --from-literal=LL_SSO_CLIENT_SECRET="$(lab_secret LL_SSO_CLIENT_SECRET)" \
  --from-literal=LL_SVKAGENT_CLIENT_SECRET="$(lab_secret LL_SVKAGENT_CLIENT_SECRET)" \
  --from-literal=LL_UNUSED_CLIENT_SECRET="$(lab_secret LL_UNUSED_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
deploy_keycloak ledgerline-identity "$LEDGERLINE_DOMAIN" https-ledgerline "$D/ledgerline/realm-ledgerline.json" identity-assertion-jwt
apply_tmpl "$D/ledgerline/research.yaml"
deny_internet ledgerline ledgerline-identity
ok "https://idp.$LEDGERLINE_DOMAIN  https://mcp.$LEDGERLINE_DOMAIN"

step "Sterling & Vance: workspace, waypoint, agent, Cross App Access"
# Ledgerline issued S&V this client secret; S&V stores it with its egress gateway.
K create secret generic ledgerline-client -n agentgateway-system \
  --from-literal=clientSecret="$(lab_secret LL_SVKAGENT_CLIENT_SECRET)" --dry-run=client -o yaml | K apply -f - >/dev/null
for f in "$D"/manifests/*.yaml; do apply_tmpl "$f"; done
# Bob's agent is a SandboxAgent; an Agent of the same name (older labs) must go first.
K delete agent bob-assistant -n sv-agents --ignore-not-found --wait >/dev/null
K apply -k "$D/agent" >/dev/null
rollout sv-mcp deploy/bob-workspace deploy/mcp-waypoint
wait_for "bob-assistant Ready" 60 5 K wait sandboxagent/bob-assistant -n sv-agents --for=condition=Ready --timeout=2s
K apply -k "$D/desk" >/dev/null
for a in meeting-prep market-brief compliance-check; do
  wait_for "$a Ready" 60 5 K wait "sandboxagent/$a" -n sv-agents --for=condition=Ready --timeout=2s
done
ok "advisor desk (sv-agents/sa/advisor-desk): meeting-prep, market-brief, compliance-check"
ok "sign in at https://kagent.$SV_DOMAIN as bob / bob-demo, chat with sv-agents/bob-assistant"
