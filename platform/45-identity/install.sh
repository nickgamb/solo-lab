#!/usr/bin/env bash
# Sterling & Vance identity: the firm's broker (Keycloak, realm sterling-vance)
# in sv-identity, published at https://idp.sterling.lab, which routes sign-ins
# to the firm's IdPs and maps them into one profile; the firm's own workforce
# IdP (Keycloak, realm workforce) in sv-workforce at https://login.sterling.lab,
# the keycloak in ENTERPRISE_IDP; its contingency IdP (realm contingency) in
# sv-contingency at https://login-dr.sterling.lab; and the client secrets each S&V component
# needs, each placed in the namespace that uses it.
# Alice's IdP is hers and is installed by demos/bob-to-alice, not here.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster

step "Sterling & Vance mesh baseline, before the firm's first workload"
# So nothing of S&V's (Keycloak here, kagent at 60, agentregistry at 70) runs
# unfenced while the layers in between install. Layer 80 owns these and
# applies them again.
apply_tmpl "$LAB_ROOT/platform/80-mesh-policy/sterling-vance.yaml"
deny_internet sv-identity sv-workforce sv-contingency kagent sv-agents sv-mcp agentregistry
ok "STRICT mTLS, identity-scoped ALLOWs and no-internet for S&V's namespaces"

step "Sterling & Vance Keycloak $KEYCLOAK_VERSION (sv-identity)"
# S&V's broker authenticates to upstream IdPs with its own key
# (private_key_jwt): a PS256 realm key used for nothing else. Client
# xaa-egress (S&V's egress) authenticates with the egress's key; the realm
# holds only its certificate. Both stable, .lab/keys.
. "$LAB_ROOT/scripts/idp.sh"
realm_signing_key sv-broker-client
realm_signing_key sv-egress-client
secret_apply sv-identity kc-secrets \
  SV_BROKER_CLIENT_KEY="$(pem_body "$LAB_STATE/keys/sv-broker-client.key")" \
  SV_BROKER_CLIENT_CERT="$(pem_body "$LAB_STATE/keys/sv-broker-client.crt")" \
  SV_EGRESS_CLIENT_CERT="$(pem_body "$LAB_STATE/keys/sv-egress-client.crt")" \
  SV_EGRESS_CLIENT_KID="$(jwks_of "$LAB_STATE/keys/sv-egress-client.crt" | jq -r '.keys[0].kid')" \
  KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret SV_KC_ADMIN_PASSWORD)" \
  SV_KAGENT_CLIENT_SECRET="$(lab_secret SV_KAGENT_CLIENT_SECRET)" \
  SV_MCP_WAYPOINT_CLIENT_SECRET="$(lab_secret SV_MCP_WAYPOINT_CLIENT_SECRET)" \
  SV_AGENTREGISTRY_CLIENT_SECRET="$(lab_secret SV_AGENTREGISTRY_CLIENT_SECRET)" \
  SV_CONTINUITY_CLIENT_SECRET="$(lab_secret SV_CONTINUITY_CLIENT_SECRET)" \
  SV_CONTINUITY_SYNC_CLIENT_SECRET="$(lab_secret SV_CONTINUITY_SYNC_CLIENT_SECRET)" \
  SV_OBSERVATORY_CLIENT_SECRET="$(lab_secret SV_OBSERVATORY_CLIENT_SECRET)"
# S&V's IdP is the enterprise IdP for Cross App Access, so it must ISSUE
# ID-JAGs: Keycloak 26.7.4 + keycloak/keycloak PR #49998 (tools/keycloak-idjag),
# tagged by a hash of its sources. Back to stock KC_IMAGE once that PR ships
# upstream.
KC_IDJAG=$(kc_idjag_image)
lab_image "$KC_IDJAG" tools/keycloak-idjag/build.sh
# Its realm names Ledgerline's authorization server as the ID-JAG audience
# (RESOURCE_AS) and lets client xaa-egress read users' stored tokens only for
# upstreams that issue ID-JAGs (ENTERPRISE_IDP), through the Identity
# Brokering API v2.
XAA_UPSTREAMS=$(xaa_upstreams_attr); export XAA_UPSTREAMS
KC_IMAGE="localhost:${LAB_REGISTRY_PORT}/$KC_IDJAG" \
  deploy_keycloak sv-identity "$SV_DOMAIN" https-sterling "$D/realm-sterling-vance.json" \
  token-exchange-standard,identity-assertion-jwt,identity-brokering-api:v2
wait_for "https://idp.$SV_DOMAIN discovery" 30 3 \
  sh -c "curl -sf --cacert '$LAB_CA_DIR/ca.crt' https://idp.$SV_DOMAIN/realms/sterling-vance/.well-known/openid-configuration >/dev/null"
ok "issuer https://idp.$SV_DOMAIN/realms/sterling-vance  (admin: see .lab/secrets.env)"

step "Sterling & Vance workforce IdP, Keycloak $KEYCLOAK_VERSION (sv-workforce)"
# The Keycloak S&V runs itself (keycloak in ENTERPRISE_IDP), apart from the
# broker: its own accounts, sessions and keys. It issues ID-JAGs for the
# broker's sign-ins like any upstream that vouches. S&V's client there
# (sterling-vance-broker) takes the broker's and the egress's keys;
# continuity-directory lets the profile sync read users.
# The broker's sign-ins there take a second factor (acr aal2): employees'
# authenticator-app (TOTP) seeds, .lab/secrets.env (make totp prints a code).
secret_apply sv-workforce kc-secrets \
  KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret SV_WORKFORCE_KC_ADMIN_PASSWORD)" \
  SV_WORKFORCE_DIRECTORY_SECRET="$(lab_secret SV_WORKFORCE_DIRECTORY_SECRET)" \
  SV_WORKFORCE_TOTP_BOB="$(lab_secret SV_WORKFORCE_TOTP_BOB)" \
  SV_WORKFORCE_TOTP_CAROL="$(lab_secret SV_WORKFORCE_TOTP_CAROL)"
mkdir -p "$LAB_STATE/realm"
workforce_realm "$D/realm-workforce.json" >"$LAB_STATE/realm/realm-workforce.json"
KC_HOST=login KC_IMAGE="localhost:${LAB_REGISTRY_PORT}/$KC_IDJAG" \
  deploy_keycloak sv-workforce "$SV_DOMAIN" https-sterling "$LAB_STATE/realm/realm-workforce.json" \
  token-exchange-standard,identity-assertion-jwt
wait_for "https://login.$SV_DOMAIN discovery" 30 3 \
  sh -c "curl -sf --cacert '$LAB_CA_DIR/ca.crt' https://login.$SV_DOMAIN/realms/workforce/.well-known/openid-configuration >/dev/null"
ok "issuer https://login.$SV_DOMAIN/realms/workforce  (bob / bob-demo + make totp; admin: see .lab/secrets.env)"

step "Sterling & Vance contingency IdP, Keycloak $KEYCLOAK_VERSION (sv-contingency)"
# The IdP S&V keeps for when its own is down (contingency in ENTERPRISE_IDP):
# its own accounts, password only (acr aal1), in its own namespace. Workloads
# that need more than a password fail closed while it carries sign-ins
# (docs/IDENTITY-CONTINUITY.md). S&V's client there takes the broker's key.
secret_apply sv-contingency kc-secrets \
  KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret SV_CONTINGENCY_KC_ADMIN_PASSWORD)"
contingency_realm "$D/realm-contingency.json" >"$LAB_STATE/realm/realm-contingency.json"
KC_HOST=login-dr \
  deploy_keycloak sv-contingency "$SV_DOMAIN" https-sterling "$LAB_STATE/realm/realm-contingency.json"
wait_for "https://login-dr.$SV_DOMAIN discovery" 30 3 \
  sh -c "curl -sf --cacert '$LAB_CA_DIR/ca.crt' https://login-dr.$SV_DOMAIN/realms/contingency/.well-known/openid-configuration >/dev/null"
ok "issuer https://login-dr.$SV_DOMAIN/realms/contingency  (bob / bob-demo; admin: see .lab/secrets.env)"

step "Client secrets, in the namespace that uses each"
# kgateway OAuth2 (edge SSO for kagent) lives with the kagent route
secret_apply kagent kagent-oidc client-secret="$(lab_secret SV_KAGENT_CLIENT_SECRET)"
# kagent's back channel for Cross App Access (the ID token, the ID-JAG):
# Keycloak issues an ID-JAG only for the app the user signed into, so the
# egress acts as kagent, the same application as the edge. Reading users'
# stored upstream tokens takes client xaa-egress and the egress's own key.
secret_apply agentgateway-system kagent-client clientSecret="$(lab_secret SV_KAGENT_CLIENT_SECRET)"
# the sv-mcp waypoint's own token-exchange client identity
secret_apply sv-mcp mcp-waypoint-oidc clientSecret="$(lab_secret SV_MCP_WAYPOINT_CLIENT_SECRET)"
ok "kagent/kagent-oidc  agentgateway-system/kagent-client  sv-mcp/mcp-waypoint-oidc"
