# shellcheck shell=bash
# Sterling & Vance's enterprise IdP chain (ENTERPRISE_IDP) and Ledgerline's
# authorization server (RESOURCE_AS), from config/lab.env. Sourced by layers
# 45 and 47 and by demos/bob (docs/IDENTITY-FLOWS.md, docs/GLUU.md).
#
#   ENTERPRISE_IDP  S&V's IdPs, in failover order, each brokered by S&V's
#                   Keycloak (the broker). Each needs <NAME>_ISSUER and
#                   <NAME>_CLIENT_ID (keycloak's and contingency's default to
#                   S&V's own, layer 45), and <NAME>_CLIENT_SECRET unless S&V
#                   uses its keys.
#   ID-JAG          Bob's ID-JAG comes from the IdP he signed in with when it
#                   issues them (gluu, keycloak); for one that doesn't
#                   (okta, auth0), the broker vouches for that sign-in.
#   RESOURCE_AS     keycloak (Ledgerline's own) or gluu (RESOURCE_AS_ISSUER).

IDP_UPSTREAMS="okta auth0 gluu keycloak contingency"  # brokered by S&V's Keycloak
IDP_ISSUES_IDJAG="gluu keycloak"          # upstreams that issue ID-JAGs
SV_ISSUER="https://idp.$SV_DOMAIN/realms/sterling-vance"
SV_CLIENT_AT_LEDGERLINE=sterling-vance-kagent

_idp_var() { local v; v="$(echo "$1" | tr '[:lower:]' '[:upper:]')_$2"; echo "${!v:-}"; }

# idp_discover <issuer>: OpenID configuration, fetched on the host. A lab
# issuer (*.<LAB_TLD>) is checked against the lab CA, not the host's store.
idp_discover() {
  local ca=()
  idp_internal "$1" && ca=(--cacert "$LAB_CA_DIR/ca.crt")
  curl -sf --max-time 15 ${ca[@]+"${ca[@]}"} "${1%/}/.well-known/openid-configuration" \
    || die "no OpenID discovery at ${1%/}/.well-known/openid-configuration"
}

# idp_names: ENTERPRISE_IDP as a list, checked: known IdPs only, each once
idp_names() {
  local n seen=" "
  for n in $(echo "$ENTERPRISE_IDP" | tr ',' ' '); do
    case " $IDP_UPSTREAMS " in *" $n "*) ;; *) die "ENTERPRISE_IDP: unknown IdP '$n' (known: $IDP_UPSTREAMS, comma-separated, lower case)" ;; esac
    case "$seen" in *" $n "*) die "ENTERPRISE_IDP: '$n' is listed twice ($ENTERPRISE_IDP)" ;; esac
    seen="$seen$n "
  done
  [ "$seen" != " " ] || die "ENTERPRISE_IDP is empty (one or more of: $IDP_UPSTREAMS)"
  echo "${seen# }" | sed 's/ $//'
}

# idp_chain: ENTERPRISE_IDP, checked, without IdPs that have no issuer
idp_chain() {
  local n out="" names
  names=$(idp_names) || exit 1
  for n in $names; do
    if [ -n "$(_idp_var "$n" ISSUER)" ]; then out="$out $n"; fi
  done
  [ -n "$out" ] || die "ENTERPRISE_IDP: no IdP with an issuer ($ENTERPRISE_IDP)"
  echo "${out# }"
}

# idp_xaa_upstreams: the IdPs in the chain that vouch for Bob themselves
# (issue his ID-JAG)
idp_xaa_upstreams() {
  local n out=""
  for n in $(idp_chain); do
    case " $IDP_ISSUES_IDJAG " in *" $n "*) out="$out $n" ;; esac
  done
  echo "${out# }"
}

# idp_internal <url>: whether it is one of the lab's own hosts (in-cluster,
# through the edge), not on the internet
idp_internal() { case "$(echo "$1" | sed -E 's#^https?://([^/:]+).*#\1#')" in *."$LAB_TLD") return 0 ;; esac; return 1; }

# ras_env: Ledgerline's authorization server
ras_env() {
  local d n
  case "$RESOURCE_AS" in
    keycloak)
      LEDGERLINE_AS_TOKEN_URL="https://idp.$LEDGERLINE_DOMAIN/realms/ledgerline/protocol/openid-connect/token"
      LEDGERLINE_AS_JWKS_URI="http://keycloak.ledgerline-identity.svc/realms/ledgerline/protocol/openid-connect/certs" ;;
    gluu)
      [ -n "$RESOURCE_AS_ISSUER" ] || die "RESOURCE_AS=gluu needs RESOURCE_AS_ISSUER in .env"
      d=$(idp_discover "$RESOURCE_AS_ISSUER")
      [ "$(echo "$d" | jq -r .issuer)" = "$RESOURCE_AS_ISSUER" ] \
        || die "RESOURCE_AS_ISSUER must match the issuer its discovery states: $(echo "$d" | jq -r .issuer)"
      for n in $(idp_chain); do
        [ "${RESOURCE_AS_ISSUER%/}" != "$(_idp_var "$n" ISSUER | sed 's#/*$##')" ] \
          || die "RESOURCE_AS_ISSUER is S&V's $n: an IdP can't vouch for Bob to itself. Ledgerline needs its own deployment"
      done
      LEDGERLINE_AS_TOKEN_URL=$(echo "$d" | jq -r .token_endpoint)
      LEDGERLINE_AS_JWKS_URI=$(echo "$d" | jq -r .jwks_uri) ;;
    *) die "RESOURCE_AS: keycloak or gluu, not '$RESOURCE_AS'" ;;
  esac
  export LEDGERLINE_AS_TOKEN_URL LEDGERLINE_AS_JWKS_URI
}

# idp_client_auth <name>: how S&V authenticates to an upstream: with its keys
# (private_key_jwt) unless the upstream was given a client secret in .env
idp_client_auth() { if [ -n "$(_idp_var "$1" CLIENT_SECRET)" ]; then echo client_secret_post; else echo private_key_jwt; fi; }

# xaa_env: the OpenID Providers S&V's egress may get Bob's ID-JAG from, in
# order, S&V's broker last as "sterling-vance" (XAA_OPS, for idtoken-exchange
# and xaa-relay), and S&V's client key at Ledgerline's AS
xaa_env() {
  local n d ops='[]'
  for n in $(idp_xaa_upstreams); do
    [ -n "$(_idp_var "$n" CLIENT_ID)" ] \
      || die "ENTERPRISE_IDP: $n needs $(echo "$n" | tr '[:lower:]' '[:upper:]')_CLIENT_ID in .env"
    d=$(idp_discover "$(_idp_var "$n" ISSUER)")
    ops=$(echo "$ops" | jq -c --arg n "$n" --argjson d "$d" --arg cid "$(_idp_var "$n" CLIENT_ID)" --arg auth "$(idp_client_auth "$n")" \
      '. + [{name: $n, issuer: $d.issuer, token_url: $d.token_endpoint, jwks_url: $d.jwks_uri, client_id: $cid, auth: $auth}]')
  done
  XAA_OPS=$(echo "$ops" | jq -c --arg iss "$SV_ISSUER" '. + [{name: "sterling-vance", issuer: $iss,
    token_url: "http://keycloak.sv-identity/realms/sterling-vance/protocol/openid-connect/token",
    jwks_url: "http://keycloak.sv-identity/realms/sterling-vance/protocol/openid-connect/certs", client_id: "kagent",
    auth: "client_secret_basic"}]')
  realm_signing_key sv-xaa-client
  realm_signing_key sv-egress-client
  SV_XAA_CLIENT_CERT=$(pem_body "$LAB_STATE/keys/sv-xaa-client.crt")
  SV_XAA_CLIENT_KID=$(xaa_client_jwks | jq -r '.keys[0].kid')
  export XAA_OPS SV_XAA_CLIENT_CERT SV_XAA_CLIENT_KID SV_CLIENT_AT_LEDGERLINE
}

# xaa_upstreams_attr: the upstreams kagent may read Bob's stored tokens for
# (Keycloak's multivalued client attribute)
xaa_upstreams_attr() { idp_xaa_upstreams | sed 's/ /##/g'; }

# xaa_secrets: S&V's credentials for its egress: its own key (client
# xaa-egress at S&V's Keycloak, S&V's client at each upstream that vouches,
# or that upstream's client secret) and its key for Ledgerline's AS
xaa_secrets() {
  local n
  for n in $IDP_UPSTREAMS; do
    case " $(idp_xaa_upstreams) " in
      *" $n "*) if [ "$(idp_client_auth "$n")" = client_secret_post ]; then
                  secret_apply agentgateway-system "op-$n" clientSecret="$(_idp_var "$n" CLIENT_SECRET)"
                fi ;;
      *) K delete secret "op-$n" -n agentgateway-system --ignore-not-found >/dev/null ;;
    esac
  done
  # the egress's own key: client xaa-egress at S&V's Keycloak, and S&V's
  # client at each upstream (beside the broker's key)
  K create secret generic egress-client-key -n agentgateway-system --from-file=signingKey="$LAB_STATE/keys/sv-egress-client.key" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
  K create secret generic xaa-client-key -n agentgateway-system --from-file=signingKey="$LAB_STATE/keys/sv-xaa-client.key" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
}

# jwks_of <cert>: the public half of a lab key, as a JWKS with its RFC 7638 kid
jwks_of() {
  openssl x509 -in "$1" -noout -modulus | cut -d= -f2 | python3 -c '
import base64, hashlib, json, sys
n = base64.urlsafe_b64encode(bytes.fromhex(sys.stdin.read().strip())).rstrip(b"=").decode()
jwk = {"e": "AQAB", "kty": "RSA", "n": n}
kid = base64.urlsafe_b64encode(hashlib.sha256(json.dumps(jwk, separators=(",", ":"), sort_keys=True).encode()).digest()).rstrip(b"=").decode()
print(json.dumps({"keys": [{**jwk, "kid": kid, "use": "sig", "alg": "RS256"}]}, indent=2))'
}
# sv_upstream_client_jwks: S&V's client at an upstream IdP: the broker's key
# (as S&V's Keycloak publishes it, PS256) and the egress's key (RS256)
sv_upstream_client_jwks() {
  local certs
  realm_signing_key sv-egress-client
  certs=$(curl -sf --cacert "$LAB_CA_DIR/ca.crt" "https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/certs") || die "no JWKS from S&V's Keycloak"
  echo "$certs" | jq --argjson egress "$(jwks_of "$LAB_STATE/keys/sv-egress-client.crt")" \
    '{keys: ([.keys[] | select(.use == "sig" and .alg == "PS256")] + $egress.keys)}'
}

# workforce_realm <realm.json>: S&V's workforce IdP, with S&V's client keys
workforce_realm() {
  jq --arg jwks "$(sv_upstream_client_jwks | jq -c .)" \
    '(.clients[] | select(.clientId == "sterling-vance-broker") | .attributes["jwks.string"]) = $jwks' "$1"
}

# contingency_realm <realm.json>: S&V's contingency IdP, with the broker's key
# (it vouches for no one, so the egress's key isn't there)
contingency_realm() {
  local certs
  certs=$(curl -sf --cacert "$LAB_CA_DIR/ca.crt" "https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/certs") || die "no JWKS from S&V's Keycloak"
  jq --arg jwks "$(echo "$certs" | jq -c '{keys: [.keys[] | select(.use == "sig" and .alg == "PS256")]}')" \
    '(.clients[] | select(.clientId == "sterling-vance-broker") | .attributes["jwks.string"]) = $jwks' "$1"
}

# xaa_client_jwks: S&V's client key at Ledgerline's AS
xaa_client_jwks() { jwks_of "$LAB_STATE/keys/sv-xaa-client.crt"; }

# ledgerline_realm <realm.json>: Ledgerline's realm, trusting each IdP that
# vouches for S&V's users, for sign-in (Ledgerline's SSO client "ledgerline"
# there, private_key_jwt) and for ID-JAGs. Each is trusted for S&V's domain
# only: a first sign-in creates the account (named by its verified email) or
# links the seat with that email.
ledgerline_realm() {
  local n d idps='[]'
  for n in $(idp_xaa_upstreams); do
    d=$(idp_discover "$(_idp_var "$n" ISSUER)")
    idps=$(echo "$idps" | jq -c --arg n "$n" --argjson d "$d" '. + [{alias: "sterling-vance-\($n)",
      displayName: "Sterling & Vance (\($n))", providerId: "oidc", enabled: true, trustEmail: true,
      storeToken: false, linkOnly: false, firstBrokerLoginFlowAlias: "enterprise first sign-in",
      config: {issuer: $d.issuer, jwksUrl: $d.jwks_uri, useJwksUrl: "true", validateSignature: "true",
        authorizationUrl: $d.authorization_endpoint, tokenUrl: $d.token_endpoint,
        clientId: "ledgerline", clientAuthMethod: "private_key_jwt", clientAssertionSigningAlg: "PS256",
        pkceEnabled: "true", pkceMethod: "S256", defaultScope: "openid email profile", syncMode: "IMPORT",
        filteredByClaim: "true", claimFilterName: "email",
        jwtAuthorizationGrantEnabled: "true", jwtAuthorizationGrantAssertionReuseAllowed: "false",
        jwtAuthorizationGrantMaxAllowedAssertionExpiration: "300"}}]')
  done
  jq --argjson idps "$idps" --arg domain "$SV_DOMAIN" '.identityProviders += $idps
    | .identityProviderMappers += ($idps | map({name: "username from email", identityProviderAlias: .alias,
        identityProviderMapper: "oidc-username-idp-mapper", config: {syncMode: "INHERIT", template: "${CLAIM.email}"}}))
    | (.identityProviders[] | select(.alias | startswith("sterling-vance")) | .config.claimFilterValue)
        = (".*@" + ($domain | gsub("\\."; "\\.")))
    | (.clients[] | select(.clientId == "sterling-vance-kagent") | .attributes["oauth2.jwt.authorization.grant.idp"])
        |= ([.] + ($idps | map(.alias)) | join("##"))' "$1"
}

# ledgerline_research: filters ledgerline/research.yaml for RESOURCE_AS=gluu:
# the MCP server and its waypoint accept tokens from Ledgerline's Gluu
ledgerline_research() {
  if [ "$RESOURCE_AS" != gluu ]; then cat; return; fi
  yq '
    (select(.kind == "MCPServer") | .spec.deployment.env) |= (
      .RESEARCH_ISSUER = strenv(RESOURCE_AS_ISSUER) | .RESEARCH_JWKS_URL = strenv(LEDGERLINE_AS_JWKS_URI))
    | (select(.metadata.name == "research-callers") | .spec.traffic.jwtAuthentication.providers[0]) |= (
      .issuer = strenv(RESOURCE_AS_ISSUER) | .jwks.remote = {"url": strenv(LEDGERLINE_AS_JWKS_URI), "cacheDuration": "5m"})'
}

# ledgerline_egress_hosts: internet hosts Ledgerline's own services reach:
# its Gluu AS's keys (MCP server), the keys of each upstream it trusts for
# ID-JAGs (its Keycloak). The lab's own hosts are reached through the edge.
ledgerline_egress_hosts() {
  local n u
  { [ "$RESOURCE_AS" = gluu ] && echo "$LEDGERLINE_AS_JWKS_URI"
    for n in $(idp_xaa_upstreams); do idp_discover "$(_idp_var "$n" ISSUER)" | jq -r .jwks_uri; done
  } | while read -r u; do idp_internal "$u" || echo "$u"; done | sed -E 's#^https://([^/:]+).*#\1#' | sort -u
}

# ledgerline_egress: one ServiceEntry per host, bound to Ledgerline's egress
# waypoint (ledgerline/egress.yaml); stale ones removed
ledgerline_egress() {
  local h hosts; hosts=$(ledgerline_egress_hosts)
  for h in $hosts; do
    K apply -f - >/dev/null <<YAML
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: $(echo "$h" | tr '.' '-')
  namespace: ledgerline-egress
  labels: {istio.io/use-waypoint: egress-waypoint, lab.solo.io/egress: ledgerline}
spec:
  hosts: [$h]
  exportTo: [., ledgerline, ledgerline-identity]
  location: MESH_EXTERNAL
  resolution: DNS
  ports: [{name: tls, number: 443, protocol: TLS}]
YAML
  done
  K get serviceentry -n ledgerline-egress -l lab.solo.io/egress=ledgerline -o json \
    | jq -r --arg keep " $(echo $hosts) " '.items[] | select(($keep | contains(" " + .spec.hosts[0] + " ")) | not) | .metadata.name' \
    | while read -r se; do K delete serviceentry "$se" -n ledgerline-egress >/dev/null; done
}

# prefer_tier <tier>: make <tier> S&V's active IdP: the tiers ahead of it in
# the chain are drained (their links kept) and it returns once the
# IdentityContinuity reports <tier> active (IDC_SWITCH_TIMEOUT seconds, 90).
# restore_tiers undoes exactly that, and runs when the calling script exits
# however it exits, so the chain is never left drained. Scripted sign-ins go
# through S&V's own IdP.
_IDP_DRAINED="" _IDP_BEFORE=""
prefer_tier() {
  local cur patch gen t=${IDC_SWITCH_TIMEOUT:-90} s
  cur=$(K get idc sterling-vance -n sv-identity -o json) || die "no IdentityContinuity sterling-vance (make layer-47)"
  echo "$cur" | jq -e --arg t "$1" '.spec.tiers | any(.name == $t)' >/dev/null || die "$1 is not in S&V's chain (ENTERPRISE_IDP)"
  # the tiers ahead of <tier> not drained already: these are the ones put back
  _IDP_DRAINED=$(echo "$cur" | jq -r --arg t "$1" '([.spec.tiers[].name] | index($t)) as $i
    | [.spec.tiers[:$i][] | select(.drain != true) | .name] | join(" ")')
  _IDP_BEFORE=$(echo "$cur" | jq -r '.status.active // empty')
  gen=$(echo "$cur" | jq -r '.metadata.generation')
  if [ -n "$_IDP_DRAINED" ]; then
    on_exit restore_tiers
    patch=$(echo "$cur" | jq -c --arg d " $_IDP_DRAINED " '[.spec.tiers | to_entries[] | select(.value.name as $n | $d | contains(" \($n) "))
      | {op: "test", path: "/spec/tiers/\(.key)/name", value: .value.name}, {op: "add", path: "/spec/tiers/\(.key)/drain", value: true}]')
    gen=$(K patch idc sterling-vance -n sv-identity --type json -p "$patch" -o jsonpath='{.metadata.generation}') \
      || die "draining $_IDP_DRAINED failed"
  fi
  s=$(date +%s)
  until idc_settled "$gen" "$1"; do
    [ $(( $(date +%s) - s )) -lt "$t" ] \
      || die "S&V did not switch to $1 in ${t}s: $(K get idc sterling-vance -n sv-identity -o json | jq -r --arg t "$1" '.status.tiers[]? | select(.name == $t) | "\(.reason // "") \(.message // "")"')"
    sleep 1
  done
  [ -z "$_IDP_DRAINED" ] || step "S&V's active IdP: $1 for these checks ($_IDP_DRAINED drained until restored)"
}
# idc_settled <generation> <idp>: the controller has acted on that spec
# generation (a status read before then may predate a drain or a restore)
# and <idp> is active.
idc_settled() {
  K get idc sterling-vance -n sv-identity -o json \
    | jq -e --argjson g "$1" --arg t "$2" '(.status.observedGeneration // 0) >= $g and .status.active == $t' >/dev/null
}
# restore_tiers [wait]: put back the tiers prefer_tier drained, by name (the
# rest of the spec, changed meanwhile or not, is left alone). With wait, until
# S&V's active IdP is the one from before (a warning if it isn't in time).
restore_tiers() {
  local cur patch gen s t=${IDC_SWITCH_TIMEOUT:-90}
  [ -n "$_IDP_DRAINED" ] || return 0
  cur=$(K get idc sterling-vance -n sv-identity -o json) || { warn "restore: no IdentityContinuity sterling-vance"; return 1; }
  patch=$(echo "$cur" | jq -c --arg d " $_IDP_DRAINED " '[.spec.tiers | to_entries[] | select((.value.name as $n | $d | contains(" \($n) ")) and .value.drain == true)
    | {op: "test", path: "/spec/tiers/\(.key)/name", value: .value.name}, {op: "remove", path: "/spec/tiers/\(.key)/drain"}]')
  gen=$(echo "$cur" | jq -r '.metadata.generation')
  if [ "$patch" != "[]" ] && ! gen=$(K patch idc sterling-vance -n sv-identity --type json -p "$patch" -o jsonpath='{.metadata.generation}'); then
    warn "could not put back $_IDP_DRAINED: kubectl -n sv-identity edit idc sterling-vance, remove their drain"; return 1
  fi
  _IDP_DRAINED=""
  [ "${1:-}" = wait ] && [ -n "$_IDP_BEFORE" ] || return 0
  s=$(date +%s)
  until idc_settled "$gen" "$_IDP_BEFORE"; do
    [ $(( $(date +%s) - s )) -lt "$t" ] || { warn "S&V's active IdP is not back on $_IDP_BEFORE after ${t}s"; return 0; }
    sleep 1
  done
}

# ledgerline_signin <user> <pass>: an S&V employee signs in to Ledgerline
# once through S&V's active IdP, as in the browser: Ledgerline links their
# seat to that IdP (or creates the account), so it can redeem that IdP's
# ID-JAGs for them.
ledgerline_signin() {
  local active verifier challenge
  active=$(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}')
  verifier=$(openssl rand -hex 32)
  challenge=$(printf '%s' "$verifier" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')
  browser_signin "https://idp.$LEDGERLINE_DOMAIN/realms/ledgerline/protocol/openid-connect/auth?client_id=account-console&response_type=code&scope=openid&redirect_uri=$(jq -rn --arg u "https://idp.$LEDGERLINE_DOMAIN/realms/ledgerline/account/" '$u|@uri')&code_challenge=$challenge&code_challenge_method=S256&kc_idp_hint=sterling-vance-$active" \
    "https://idp.$LEDGERLINE_DOMAIN/realms/ledgerline/account/" "$1" "$2" >/dev/null
}

# a malformed ENTERPRISE_IDP stops whatever sources this, before it changes anything
idp_names >/dev/null
