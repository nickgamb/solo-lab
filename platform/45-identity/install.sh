#!/usr/bin/env bash
# Sterling & Vance identity: the firm's Keycloak (realm sterling-vance) in
# sv-identity, published at https://idp.sterling.lab, plus the client secrets
# each S&V component needs, each placed in the namespace that uses it.
# Alice's IdP is hers and is installed by demos/bob-to-alice, not here.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster

step "Sterling & Vance mesh baseline, before the firm's first workload"
# So nothing of S&V's (Keycloak here, kagent at 60, agentregistry at 70) runs
# unfenced while the layers in between install. Layer 80 owns these and
# applies them again.
apply_tmpl "$LAB_ROOT/platform/80-mesh-policy/sterling-vance.yaml"
deny_internet sv-identity kagent sv-agents sv-mcp agentregistry
ok "STRICT mTLS, identity-scoped ALLOWs and no-internet for S&V's namespaces"

step "Sterling & Vance Keycloak $KEYCLOAK_VERSION (sv-identity)"
K create secret generic kc-secrets -n sv-identity \
  --from-literal=KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  --from-literal=KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret SV_KC_ADMIN_PASSWORD)" \
  --from-literal=SV_KAGENT_CLIENT_SECRET="$(lab_secret SV_KAGENT_CLIENT_SECRET)" \
  --from-literal=SV_MCP_WAYPOINT_CLIENT_SECRET="$(lab_secret SV_MCP_WAYPOINT_CLIENT_SECRET)" \
  --from-literal=SV_AGENTREGISTRY_CLIENT_SECRET="$(lab_secret SV_AGENTREGISTRY_CLIENT_SECRET)" \
  --from-literal=SV_CONTINUITY_CLIENT_SECRET="$(lab_secret SV_CONTINUITY_CLIENT_SECRET)" \
  --from-literal=SV_OBSERVATORY_CLIENT_SECRET="$(lab_secret SV_OBSERVATORY_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# S&V's IdP is the enterprise IdP for Cross App Access, so it must ISSUE
# ID-JAGs: Keycloak 26.7.4 + keycloak/keycloak PR #49998 (tools/keycloak-idjag).
# Back to stock KC_IMAGE once that PR ships upstream.
lab_image lab/keycloak-idjag:${KEYCLOAK_VERSION}-pr49998 tools/keycloak-idjag/build.sh
KC_IMAGE="localhost:${LAB_REGISTRY_PORT}/lab/keycloak-idjag:${KEYCLOAK_VERSION}-pr49998" \
  deploy_keycloak sv-identity "$SV_DOMAIN" https-sterling "$D/realm-sterling-vance.json" \
  token-exchange-standard,identity-assertion-jwt
wait_for "https://idp.$SV_DOMAIN discovery" 30 3 \
  sh -c "curl -sf --cacert '$LAB_CA_DIR/ca.crt' https://idp.$SV_DOMAIN/realms/sterling-vance/.well-known/openid-configuration >/dev/null"
ok "issuer https://idp.$SV_DOMAIN/realms/sterling-vance  (admin: see .lab/secrets.env)"

step "Client secrets, in the namespace that uses each"
# kgateway OAuth2 (edge SSO for kagent) lives with the kagent route
K create secret generic kagent-oidc -n kagent \
  --from-literal=client-secret="$(lab_secret SV_KAGENT_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# agentgateway runs Cross App Access as kagent's back-channel: the requesting
# app in XAA is the app Bob signed into (kagent), so it authenticates as kagent.
K create secret generic kagent-client -n agentgateway-system \
  --from-literal=clientSecret="$(lab_secret SV_KAGENT_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# the sv-mcp waypoint's own token-exchange client identity
K create secret generic mcp-waypoint-oidc -n sv-mcp \
  --from-literal=clientSecret="$(lab_secret SV_MCP_WAYPOINT_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
ok "kagent/kagent-oidc  agentgateway-system/kagent-client  sv-mcp/mcp-waypoint-oidc"
