#!/usr/bin/env bash
# Story 1 (Bob): Bob's agent, his workspace (kmcp) behind an agentgateway
# waypoint with RFC 8693 delegation, and Ledgerline Research via Cross App
# Access (ID-JAG). Needs the platform (make platform). Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
# who vouches for Bob (ENTERPRISE_IDP) and who redeems it for Ledgerline
# (RESOURCE_AS): scripts/idp.sh
. "$LAB_ROOT/scripts/idp.sh"
ras_env; xaa_env

step "Images (local registry)"
# tagged by a hash of their source (lab_build)
BOB_WORKSPACE_IMAGE=$(lab_build sv/bob-workspace "$D/mcp/bob-workspace"); export BOB_WORKSPACE_IMAGE
LEDGERLINE_RESEARCH_IMAGE=$(lab_build ledgerline/research-mcp "$D/ledgerline/mcp"); export LEDGERLINE_RESEARCH_IMAGE
IDTOKEN_EXCHANGE_IMAGE=$(lab_build lab/idtoken-exchange "$LAB_ROOT/apps/idtoken-exchange"); export IDTOKEN_EXCHANGE_IMAGE
MCP_GUARD_IMAGE=$(lab_build lab/mcp-guard "$LAB_ROOT/apps/mcp-guard"); export MCP_GUARD_IMAGE
ok "$BOB_WORKSPACE_IMAGE  $LEDGERLINE_RESEARCH_IMAGE"
ok "$IDTOKEN_EXCHANGE_IMAGE"
ok "$MCP_GUARD_IMAGE"
TOOLBOX_IMAGE=$(lab_build lab/toolbox "$LAB_ROOT/tools/toolbox")
ok "$TOOLBOX_IMAGE   (probe pods for the checks)"

step "Ledgerline Research (its own IdP, MCP server, Istio waypoint)"
# S&V registered its public key with Ledgerline (private_key_jwt); Ledgerline
# signs in to S&V's IdPs with its own key (its SSO client there)
realm_signing_key ledgerline-sso-client
secret_apply ledgerline-identity kc-secrets \
  KC_BOOTSTRAP_ADMIN_USERNAME=admin KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret LL_KC_ADMIN_PASSWORD)" \
  SV_XAA_CLIENT_CERT="$SV_XAA_CLIENT_CERT" SV_XAA_CLIENT_KID="$SV_XAA_CLIENT_KID" \
  LL_SSO_CLIENT_KEY="$(pem_body "$LAB_STATE/keys/ledgerline-sso-client.key")" \
  LL_SSO_CLIENT_CERT="$(pem_body "$LAB_STATE/keys/ledgerline-sso-client.crt")"
# its Keycloak trusts each of S&V's IdPs that vouch, for S&V's domain only
mkdir -p "$LAB_STATE/realm"
ledgerline_realm "$D/ledgerline/realm-ledgerline.json" >"$LAB_STATE/realm/realm-ledgerline.json"
deploy_keycloak ledgerline-identity "$LEDGERLINE_DOMAIN" https-ledgerline "$LAB_STATE/realm/realm-ledgerline.json" identity-assertion-jwt
render "$D/ledgerline/research.yaml" | ledgerline_research | K apply -f - >/dev/null
apply_tmpl "$D/ledgerline/identity.yaml" "$D/ledgerline/egress.yaml"
deny_internet ledgerline ledgerline-identity
ledgerline_egress
K delete networkpolicy research-to-as -n ledgerline --ignore-not-found >/dev/null   # earlier labs
# earlier labs: an Istio waypoint in front of Ledgerline's MCP server
K delete gateway waypoint -n ledgerline --ignore-not-found >/dev/null
K delete requestauthentication ledgerline-tokens -n ledgerline --ignore-not-found >/dev/null
K delete authorizationpolicy research-access pods-only-from-waypoint -n ledgerline --ignore-not-found >/dev/null
ok "https://mcp.$LEDGERLINE_DOMAIN  authorization server: ${RESOURCE_AS} ($LEDGERLINE_AS_ISSUER)"

# Ledgerline onboards S&V's people: its Bob is S&V's Bob, by the broker
# account the directory sync made for him from S&V's primary IdP (the subject
# of the ID-JAGs S&V's broker vouches with). His links to the IdPs that vouch
# themselves come from his first sign-in to Ledgerline through each.
bid=$(kc_admin sv-identity SV_KC_ADMIN_PASSWORD sterling-vance GET "/users?exact=true&briefRepresentation=true&email=bob%40$SV_DOMAIN" | jq -r '.[0].id // empty')
lid=$(kc_admin ledgerline-identity LL_KC_ADMIN_PASSWORD ledgerline GET "/users?exact=true&briefRepresentation=true&email=bob%40$SV_DOMAIN" | jq -r '.[0].id // empty')
if [ -n "$bid" ] && [ -n "$lid" ]; then
  kc_admin ledgerline-identity LL_KC_ADMIN_PASSWORD ledgerline DELETE "/users/$lid/federated-identity/sterling-vance" -o /dev/null
  jq -nc --arg u "$bid" --arg n "bob@$SV_DOMAIN" '{identityProvider: "sterling-vance", userId: $u, userName: $n}' \
    | kc_admin ledgerline-identity LL_KC_ADMIN_PASSWORD ledgerline POST "/users/$lid/federated-identity/sterling-vance" \
        -H 'content-type: application/json' --data @- -o /dev/null
  ok "Ledgerline's Bob linked to S&V's broker account for bob@$SV_DOMAIN"
else
  warn "no S&V broker account for bob@$SV_DOMAIN yet (the directory sync makes it from S&V's primary IdP): Ledgerline can't link him"
fi

step "Sterling & Vance: workspace, waypoint, agent, Cross App Access"
# S&V's keys (or a client secret) for each upstream that vouches for Bob and
# for Ledgerline, kept with its egress gateway
xaa_secrets
K delete secret ledgerline-client -n agentgateway-system --ignore-not-found >/dev/null   # earlier labs: a shared secret
apply_tmpl "$D"/manifests/*.yaml
# Cross App Access on ai-gateway: a backend per IdP that may vouch for Bob
xaa_gateway_apply "$D/manifests"
# earlier labs: the token requests went through xaa-relay
K delete deploy,service,serviceaccount -n agentgateway-system xaa-relay --ignore-not-found >/dev/null
K delete authorizationpolicy xaa-relay-callers -n agentgateway-system --ignore-not-found >/dev/null
K delete networkpolicy no-internet-xaa-relay -n agentgateway-system --ignore-not-found >/dev/null
K delete "$AGW_BACKEND_KIND" xaa-ledgerline -n agentgateway-system --ignore-not-found >/dev/null
# Bob's agent is a SandboxAgent; an Agent of the same name (older labs) must go first.
K delete agent bob-assistant -n sv-agents --ignore-not-found --wait >/dev/null
apply_kustomize "$D/agent"
rollout sv-mcp deploy/mcp-guard deploy/bob-workspace deploy/mcp-waypoint
rollout agentgateway-system deploy/idtoken-exchange
wait_for "bob-assistant Ready" 60 5 K wait sandboxagent/bob-assistant -n sv-agents --for=condition=Ready --timeout=2s
apply_kustomize "$D/desk"
for a in meeting-prep market-brief compliance-check; do
  wait_for "$a Ready" 60 5 K wait "sandboxagent/$a" -n sv-agents --for=condition=Ready --timeout=2s
done
ok "advisor desk (sv-agents/sa/advisor-desk): meeting-prep, market-brief, compliance-check"
ok "sign in at https://kagent.$SV_DOMAIN as bob / bob-demo (and make totp at S&V's own Keycloak), chat with sv-agents/bob-assistant"
