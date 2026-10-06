# shellcheck shell=bash
# Sterling & Vance's enterprise IdP chain (ENTERPRISE_IDP) and Ledgerline's
# authorization server (RESOURCE_AS), from config/lab.env. Sourced by layers
# 45 and 47 and by demos/bob (docs/IDENTITY-FLOWS.md, docs/GLUU-INTEROP.md).
#
#   ENTERPRISE_IDP  upstreams brokered by S&V's Keycloak, in failover order,
#                   then keycloak (S&V's own accounts). Each upstream needs
#                   <NAME>_ISSUER, <NAME>_CLIENT_ID, <NAME>_CLIENT_SECRET.
#   ID-JAG          Bob's ID-JAG comes from the IdP he signs in with when it
#                   issues them (gluu). Past the first upstream that doesn't
#                   (auth0), S&V's Keycloak vouches for whoever signed in.
#   RESOURCE_AS     keycloak (Ledgerline's own) or gluu (RESOURCE_AS_ISSUER).

IDP_UPSTREAMS="auth0 gluu"   # brokered by S&V's Keycloak
IDP_ISSUES_IDJAG="gluu"      # upstreams that issue ID-JAGs
SV_ISSUER="https://idp.$SV_DOMAIN/realms/sterling-vance"
SV_CLIENT_AT_LEDGERLINE=sterling-vance-kagent

_idp_var() { local v; v="$(echo "$1" | tr '[:lower:]' '[:upper:]')_$2"; echo "${!v:-}"; }

# idp_discover <issuer>: OpenID configuration, fetched on the host
idp_discover() {
  curl -sf --max-time 15 "${1%/}/.well-known/openid-configuration" \
    || die "no OpenID discovery at ${1%/}/.well-known/openid-configuration"
}

# idp_chain: ENTERPRISE_IDP, checked, without upstreams that have no issuer
idp_chain() {
  local n last="" seen=" " out=""
  for n in $(echo "$ENTERPRISE_IDP" | tr ',' ' '); do
    case " $IDP_UPSTREAMS keycloak " in *" $n "*) ;; *) die "ENTERPRISE_IDP: unknown IdP '$n' (known: $IDP_UPSTREAMS keycloak)" ;; esac
    case "$seen" in *" $n "*) die "ENTERPRISE_IDP: '$n' is listed twice" ;; esac
    seen="$seen$n "; last=$n
    if [ "$n" = keycloak ] || [ -n "$(_idp_var "$n" ISSUER)" ]; then out="$out $n"; fi
  done
  [ "$last" = keycloak ] || die "ENTERPRISE_IDP must end with keycloak, S&V's own accounts: the tier every other one fails over to ($ENTERPRISE_IDP)"
  echo "${out# }"
}

# idp_xaa_upstreams: the upstreams at the head of the chain that vouch for
# Bob themselves (issue his ID-JAG)
idp_xaa_upstreams() {
  local n out=""
  for n in $(idp_chain); do
    case " $IDP_ISSUES_IDJAG " in *" $n "*) out="$out $n" ;; *) break ;; esac
  done
  echo "${out# }"
}

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
        [ "$n" = keycloak ] && continue
        [ "${RESOURCE_AS_ISSUER%/}" != "$(_idp_var "$n" ISSUER | sed 's#/*$##')" ] \
          || die "RESOURCE_AS_ISSUER is S&V's $n: an IdP can't vouch for Bob to itself. Ledgerline needs its own deployment"
      done
      LEDGERLINE_AS_TOKEN_URL=$(echo "$d" | jq -r .token_endpoint)
      LEDGERLINE_AS_JWKS_URI=$(echo "$d" | jq -r .jwks_uri) ;;
    *) die "RESOURCE_AS: keycloak or gluu, not '$RESOURCE_AS'" ;;
  esac
  export LEDGERLINE_AS_TOKEN_URL LEDGERLINE_AS_JWKS_URI
}

# xaa_env: the OpenID Providers S&V's egress may get Bob's ID-JAG from, in
# order, S&V's Keycloak last (XAA_OPS, for idtoken-exchange and xaa-relay),
# and S&V's client key at Ledgerline's AS
xaa_env() {
  local n d ops='[]'
  for n in $(idp_xaa_upstreams); do
    [ -n "$(_idp_var "$n" CLIENT_ID)" ] && [ -n "$(_idp_var "$n" CLIENT_SECRET)" ] \
      || die "ENTERPRISE_IDP: $n needs $(echo "$n" | tr '[:lower:]' '[:upper:]')_CLIENT_ID and _CLIENT_SECRET in .env"
    d=$(idp_discover "$(_idp_var "$n" ISSUER)")
    ops=$(echo "$ops" | jq -c --arg n "$n" --argjson d "$d" --arg cid "$(_idp_var "$n" CLIENT_ID)" \
      '. + [{name: $n, issuer: $d.issuer, token_url: $d.token_endpoint, jwks_url: $d.jwks_uri, client_id: $cid}]')
  done
  XAA_OPS=$(echo "$ops" | jq -c --arg iss "$SV_ISSUER" '. + [{name: "keycloak", issuer: $iss,
    token_url: "http://keycloak.sv-identity/realms/sterling-vance/protocol/openid-connect/token",
    jwks_url: "http://keycloak.sv-identity/realms/sterling-vance/protocol/openid-connect/certs", client_id: "kagent"}]')
  realm_signing_key sv-xaa-client
  SV_XAA_CLIENT_CERT=$(pem_body "$LAB_STATE/keys/sv-xaa-client.crt")
  SV_XAA_CLIENT_KID=$(xaa_client_jwks | jq -r '.keys[0].kid')
  export XAA_OPS SV_XAA_CLIENT_CERT SV_XAA_CLIENT_KID SV_CLIENT_AT_LEDGERLINE
}

# xaa_upstreams_attr: the upstreams kagent may read Bob's stored tokens for
# (Keycloak's multivalued client attribute)
xaa_upstreams_attr() { idp_xaa_upstreams | sed 's/ /##/g'; }

# xaa_secrets: S&V's credentials for its egress: its client secret at each
# upstream OP, and its private_key_jwt key for Ledgerline's AS
xaa_secrets() {
  local n
  for n in $IDP_UPSTREAMS; do
    case " $(idp_xaa_upstreams) " in
      *" $n "*) K create secret generic "op-$n" -n agentgateway-system \
                  --from-literal=clientSecret="$(_idp_var "$n" CLIENT_SECRET)" --dry-run=client -o yaml | K apply -f - >/dev/null ;;
      *) K delete secret "op-$n" -n agentgateway-system --ignore-not-found >/dev/null ;;
    esac
  done
  K create secret generic xaa-client-key -n agentgateway-system --from-file=signingKey="$LAB_STATE/keys/sv-xaa-client.key" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
}

# xaa_client_jwks: the public half of S&V's client key, as a JWKS with its
# RFC 7638 kid: what Ledgerline's AS registers for S&V's client
xaa_client_jwks() {
  openssl x509 -in "$LAB_STATE/keys/sv-xaa-client.crt" -noout -modulus | cut -d= -f2 | python3 -c '
import base64, hashlib, json, sys
n = base64.urlsafe_b64encode(bytes.fromhex(sys.stdin.read().strip())).rstrip(b"=").decode()
jwk = {"e": "AQAB", "kty": "RSA", "n": n}
kid = base64.urlsafe_b64encode(hashlib.sha256(json.dumps(jwk, separators=(",", ":"), sort_keys=True).encode()).digest()).rstrip(b"=").decode()
print(json.dumps({"keys": [{**jwk, "kid": kid, "use": "sig", "alg": "RS256"}]}, indent=2))'
}

# ledgerline_realm <realm.json>: Ledgerline's realm, also trusting each
# upstream that vouches for S&V's users (as an ID-JAG issuer)
ledgerline_realm() {
  local n d idps='[]'
  for n in $(idp_xaa_upstreams); do
    d=$(idp_discover "$(_idp_var "$n" ISSUER)")
    idps=$(echo "$idps" | jq -c --arg n "$n" --argjson d "$d" '. + [{alias: "sterling-vance-\($n)",
      displayName: "Sterling & Vance (\($n))", providerId: "oidc", enabled: true, trustEmail: true,
      storeToken: false, linkOnly: true, config: {issuer: $d.issuer, jwksUrl: $d.jwks_uri, useJwksUrl: "true",
        validateSignature: "true", authorizationUrl: $d.authorization_endpoint, tokenUrl: $d.token_endpoint,
        clientId: "ledgerline-sso", jwtAuthorizationGrantEnabled: "true",
        jwtAuthorizationGrantAssertionReuseAllowed: "false", jwtAuthorizationGrantMaxAllowedAssertionExpiration: "300"}}]')
  done
  jq --argjson idps "$idps" '.identityProviders += $idps
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
    | (select(.kind == "RequestAuthentication") | .spec.jwtRules[0]) |= (
      .issuer = strenv(RESOURCE_AS_ISSUER) | .jwksUri = strenv(LEDGERLINE_AS_JWKS_URI))
    | (select(.kind == "AuthorizationPolicy" and .metadata.name == "research-access") | .spec.rules[0].from[0].source.requestPrincipals)
        = [strenv(RESOURCE_AS_ISSUER) + "/*"]'
}

# ledgerline_egress_hosts: internet hosts Ledgerline's own services reach:
# its Gluu AS's keys (MCP server), the keys of each upstream it trusts for
# ID-JAGs (its Keycloak)
ledgerline_egress_hosts() {
  local n
  { [ "$RESOURCE_AS" = gluu ] && echo "$LEDGERLINE_AS_JWKS_URI"
    for n in $(idp_xaa_upstreams); do idp_discover "$(_idp_var "$n" ISSUER)" | jq -r .jwks_uri; done
  } | sed -E 's#^https://([^/:]+).*#\1#' | sort -u
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

# kc_admin <ns> <admin password secret>: an admin token for a party's
# Keycloak (master realm) over a port-forward; prints "<port> <token>"
kc_admin() {
  local lp tok; lp=$(free_port)
  port_forward "$1" keycloak "$lp" 80
  tok=$(_P="$(lab_secret "$2")" jq -rn '{grant_type: "password", client_id: "admin-cli", username: "admin", password: $ENV._P}
      | to_entries | map("\(.key)=\(.value | @uri)") | join("&")' \
    | curl -s "http://127.0.0.1:$lp/realms/master/protocol/openid-connect/token" --data @- | jq -r .access_token)
  echo "$lp $tok"
}

# _kc_api <port> <admin token> <path> [curl args]: the token goes to curl on a
# file descriptor, never the command line
_kc_api() {
  local p=$1 t=$2 path=$3; shift 3
  curl -s -K <(printf 'header = "Authorization: Bearer %s"\n' "$t") "$@" "http://127.0.0.1:$p$path"
}

# ledgerline_link_bob: Ledgerline provisions Bob's account for each upstream
# that vouches for him, keyed by his subject there (the step a SaaS takes at
# onboarding or first sign-in), once Bob has signed in to S&V through it.
# Prints the upstreams still waiting for that sign-in.
ledgerline_link_bob() {
  local n svp svt llp llt svid llid sub
  [ -n "$(idp_xaa_upstreams)" ] || return 0
  read -r svp svt <<<"$(kc_admin sv-identity SV_KC_ADMIN_PASSWORD)"
  read -r llp llt <<<"$(kc_admin ledgerline-identity LL_KC_ADMIN_PASSWORD)"
  svid=$(_kc_api "$svp" "$svt" "/admin/realms/sterling-vance/users?username=bob&exact=true" | jq -r '.[0].id')
  llid=$(_kc_api "$llp" "$llt" "/admin/realms/ledgerline/users?username=bob@sterling.lab&exact=true" | jq -r '.[0].id')
  for n in $(idp_xaa_upstreams); do
    sub=$(_kc_api "$svp" "$svt" "/admin/realms/sterling-vance/users/$svid/federated-identity" \
      | jq -r --arg n "$n" '.[] | select(.identityProvider == $n) | .userId')
    if [ -z "$sub" ]; then echo "$n"; continue; fi
    jq -n --arg sub "$sub" '{userId: $sub, userName: "bob"}' | _kc_api "$llp" "$llt" \
      "/admin/realms/ledgerline/users/$llid/federated-identity/sterling-vance-$n" -o /dev/null -X POST \
      -H 'Content-Type: application/json' --data @-
  done
}
