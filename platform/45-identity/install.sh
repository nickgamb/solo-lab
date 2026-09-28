#!/usr/bin/env bash
# Sterling & Vance identity: the firm's Keycloak (realm sterling-vance) in
# sv-identity, published at https://idp.sterling.lab, plus the client secrets
# each S&V component needs, each placed in the namespace that uses it.
# Alice's IdP is hers and is installed by demos/bob-to-alice, not here.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster

step "Sterling & Vance Keycloak $KEYCLOAK_VERSION (sv-identity)"
K create secret generic kc-secrets -n sv-identity \
  --from-literal=KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  --from-literal=KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret SV_KC_ADMIN_PASSWORD)" \
  --from-literal=SV_KAGENT_CLIENT_SECRET="$(lab_secret SV_KAGENT_CLIENT_SECRET)" \
  --from-literal=SV_AIGW_CLIENT_SECRET="$(lab_secret SV_AIGW_CLIENT_SECRET)" \
  --from-literal=SV_MCP_WAYPOINT_CLIENT_SECRET="$(lab_secret SV_MCP_WAYPOINT_CLIENT_SECRET)" \
  --from-literal=SV_GRAFANA_CLIENT_SECRET="$(lab_secret SV_GRAFANA_CLIENT_SECRET)" \
  --from-literal=SV_UNUSED_CLIENT_SECRET="$(lab_secret SV_UNUSED_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
deploy_keycloak sv-identity "$SV_DOMAIN" https-sterling "$D/realm-sterling-vance.json"
wait_for "https://idp.$SV_DOMAIN discovery" 30 3 \
  sh -c "curl -sf https://idp.$SV_DOMAIN/realms/sterling-vance/.well-known/openid-configuration >/dev/null"
ok "issuer https://idp.$SV_DOMAIN/realms/sterling-vance  (admin: see .lab/secrets.env)"

step "Client secrets, in the namespace that uses each"
# kgateway OAuth2 (edge SSO for kagent) lives with the kagent route
K create secret generic kagent-oidc -n kagent \
  --from-literal=client-secret="$(lab_secret SV_KAGENT_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# agentgateway's token-exchange client identity
K create secret generic ai-gateway-oidc -n agentgateway-system \
  --from-literal=clientSecret="$(lab_secret SV_AIGW_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# the sv-mcp waypoint's own token-exchange client identity
K create secret generic mcp-waypoint-oidc -n sv-mcp \
  --from-literal=clientSecret="$(lab_secret SV_MCP_WAYPOINT_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# Grafana SSO (platform UI, S&V workforce identities)
K create secret generic grafana-oidc -n observability \
  --from-literal=client-secret="$(lab_secret SV_GRAFANA_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
ok "kagent/kagent-oidc  agentgateway-system/ai-gateway-oidc  sv-mcp/mcp-waypoint-oidc  observability/grafana-oidc"
