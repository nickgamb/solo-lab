# shellcheck shell=bash
# Sterling & Vance's enterprise IdP chain (ENTERPRISE_IDP) and Ledgerline's
# authorization server (RESOURCE_AS), from config/lab.env. Sourced by layers
# 45 and 47 and by demos/bob (docs/IDENTITY-FLOWS.md, docs/GLUU.md).
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

# idp_client_auth <name>: how S&V authenticates to an upstream: with its keys
# (private_key_jwt) unless the upstream was given a client secret in .env
idp_client_auth() { if [ -n "$(_idp_var "$1" CLIENT_SECRET)" ]; then echo client_secret_post; else echo private_key_jwt; fi; }

# xaa_env: the OpenID Providers S&V's egress may get Bob's ID-JAG from, in
# order, S&V's Keycloak last (XAA_OPS, for idtoken-exchange and xaa-relay),
# and S&V's client key at Ledgerline's AS
xaa_env() {
  local n d ops='[]'
  for n in $(idp_xaa_upstreams); do
    [ -n "$(_idp_var "$n" CLIENT_ID)" ] \
      || die "ENTERPRISE_IDP: $n needs $(echo "$n" | tr '[:lower:]' '[:upper:]')_CLIENT_ID in .env"
    d=$(idp_discover "$(_idp_var "$n" ISSUER)")
    ops=$(echo "$ops" | jq -c --arg n "$n" --argjson d "$d" --arg cid "$(_idp_var "$n" CLIENT_ID)" --arg auth "$(idp_client_auth "$n")" \
      '. + [{name: $n, issuer: $d.issuer, token_url: $d.token_endpoint, jwks_url: $d.jwks_uri, client_id: $cid, auth: $auth}]')
  done
  XAA_OPS=$(echo "$ops" | jq -c --arg iss "$SV_ISSUER" '. + [{name: "keycloak", issuer: $iss,
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
      *" $n "*) [ "$(idp_client_auth "$n")" = client_secret_post ] && K create secret generic "op-$n" -n agentgateway-system \
                  --from-literal=clientSecret="$(_idp_var "$n" CLIENT_SECRET)" --dry-run=client -o yaml | K apply -f - >/dev/null ;;
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
