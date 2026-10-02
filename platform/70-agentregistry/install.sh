#!/usr/bin/env bash
# agentregistry: Sterling & Vance's catalog of agents, MCP servers and skills.
# It may only manage kagent resources in sv-agents.
#   oss:        0.4.0, which has no authentication of its own, behind S&V's
#               SSO at the kgateway edge (https://registry.sterling.lab)
#   enterprise: Solo Enterprise for agentregistry 2026.9.0, which signs people
#               in against S&V's Keycloak itself
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
ED=$AGENTREGISTRY_EDITION
step "agentregistry $AGENTREGISTRY_VERSION ($ED)"
if [ "$ED" = enterprise ]; then
  [ -n "$SOLO_AGENTREGISTRY_LICENSE_KEY" ] || warn "no SOLO_AGENTREGISTRY_LICENSE_KEY (or SOLO_LICENSE_KEY) in .env: agentregistry-enterprise runs, and logs that it's unlicensed"
  printf '%s' "$SOLO_AGENTREGISTRY_LICENSE_KEY" | K create secret generic enterprise-agentregistry-license -n agentregistry \
    --from-file=enterprise-agentregistry-license-key=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
  AR_CLICKHOUSE_PASSWORD=$(lab_secret AR_CLICKHOUSE_PASSWORD); export AR_CLICKHOUSE_PASSWORD
fi
values_for "$D" values "$ED"
helm_up agentregistry "$AGENTREGISTRY_CHART" "$AGENTREGISTRY_VERSION" agentregistry ${VALS[@]+"${VALS[@]}"}
if [ "$ED" = enterprise ]; then
  # it signs people in itself: an edge OAuth2 filter (OSS) would take over its route
  K delete trafficpolicy agentregistry-sso -n agentregistry --ignore-not-found >/dev/null
  K delete gatewayextension agentregistry-sso -n agentregistry --ignore-not-found >/dev/null
  apply_tmpl "$D/enterprise.yaml"
  ok "https://registry.${SV_DOMAIN} (its own sign-in: S&V Keycloak, client agentregistry-ui)"
else
  K create secret generic agentregistry-oidc -n agentregistry \
    --from-literal=client-secret="$(lab_secret SV_AGENTREGISTRY_CLIENT_SECRET)" --dry-run=client -o yaml | K apply -f - >/dev/null
  apply_tmpl "$D/edge-sso.yaml"
  ok "https://registry.${SV_DOMAIN} (S&V SSO at the edge)"
fi
