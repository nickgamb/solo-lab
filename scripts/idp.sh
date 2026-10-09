# shellcheck shell=bash
# Sterling & Vance's enterprise IdP chain (ENTERPRISE_IDP) and Ledgerline's
# authorization server (RESOURCE_AS), from config/lab.env. Sourced by layers
# 45 and 47 and by demos/bob (docs/IDENTITY-FLOWS.md, docs/GLUU.md).
#
#   ENTERPRISE_IDP  S&V's IdPs, in failover order, each brokered by S&V's
#                   Keycloak (the broker), each configured by its NAME_*
#                   settings (config/lab.env, docs/IDPS.md).
#   ID-JAG          S&V's broker vouches for every S&V user (one issuer for
#                   partners), whichever IdP signed them in.
#   RESOURCE_AS     Ledgerline's authorization server, by its LEDGERLINE_NAME_*
#                   settings: Ledgerline's own Keycloak (in the lab) or another.
SV_ISSUER="https://idp.$SV_DOMAIN/realms/sterling-vance"

_idp_var() { local v; v="$(echo "$1" | tr '[:lower:]' '[:upper:]')_$2"; echo "${!v:-}"; }
# idp_setting <name> <setting> <default>: a setting, or its default
idp_setting() { local v; v=$(_idp_var "$1" "$2"); printf '%s' "${v:-$3}"; }
# idp_secret <name> <setting>: a credential setting's value; lab:<SECRET> is a
# secret the lab generates (.lab/secrets.env)
idp_secret() {
  local v; v=$(_idp_var "$1" "$2")
  case "$v" in lab:*) lab_secret "${v#lab:}" ;; *) printf '%s' "$v" ;; esac
}
# idp_discover <issuer>: OpenID configuration, fetched on the host. A lab
# issuer (*.<LAB_TLD>) is checked against the lab CA, not the host's store.
idp_discover() {
  local ca=()
  idp_internal "$1" && ca=(--cacert "$LAB_CA_DIR/ca.crt")
  curl -sf --max-time 15 ${ca[@]+"${ca[@]}"} "${1%/}/.well-known/openid-configuration" \
    || die "no OpenID discovery at ${1%/}/.well-known/openid-configuration"
}

# idp_names: ENTERPRISE_IDP as a list, checked: names (lower case letters
# and digits), each once
idp_names() {
  local n seen=" "
  for n in $(echo "$ENTERPRISE_IDP" | tr ',' ' '); do
    case "$n" in [a-z]*) ;; *) die "ENTERPRISE_IDP: '$n' isn't a name (lower case letters and digits, comma-separated)" ;; esac
    case "$n" in *[!a-z0-9]*) die "ENTERPRISE_IDP: '$n' isn't a name (lower case letters and digits, comma-separated)" ;; esac
    case "$seen" in *" $n "*) die "ENTERPRISE_IDP: '$n' is listed twice ($ENTERPRISE_IDP)" ;; esac
    seen="$seen$n "
  done
  [ "$seen" != " " ] || die "ENTERPRISE_IDP is empty"
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

# idp_internal <url>: whether it is one of the lab's own hosts (in-cluster,
# through the edge), not on the internet
idp_internal() { case "$(echo "$1" | sed -E 's#^https?://([^/:]+).*#\1#')" in *."$LAB_TLD") return 0 ;; esac; return 1; }

# ras_external: whether Ledgerline's AS is one the lab reaches over the
# internet, not its own Keycloak
ras_external() { ! idp_internal "$LEDGERLINE_AS_ISSUER"; }

# ras_env: Ledgerline's authorization server's token endpoint and keys
ras_env() {
  local d
  [ -n "$LEDGERLINE_AS_ISSUER" ] \
    || die "RESOURCE_AS=$RESOURCE_AS needs LEDGERLINE_$(echo "$RESOURCE_AS" | tr '[:lower:]' '[:upper:]')_ISSUER in .env"
  if ! ras_external; then
    # Ledgerline's own Keycloak (demos/bob installs it after this runs)
    LEDGERLINE_AS_TOKEN_URL="${LEDGERLINE_AS_ISSUER%/}/protocol/openid-connect/token"
    LEDGERLINE_AS_JWKS_URI="http://keycloak.ledgerline-identity.svc/realms/ledgerline/protocol/openid-connect/certs"
  else
    d=$(idp_discover "$LEDGERLINE_AS_ISSUER")
    [ "$(echo "$d" | jq -r .issuer)" = "$LEDGERLINE_AS_ISSUER" ] \
      || die "Ledgerline's AS issuer must match the issuer its discovery states: $(echo "$d" | jq -r .issuer)"
    LEDGERLINE_AS_TOKEN_URL=$(echo "$d" | jq -r .token_endpoint)
    LEDGERLINE_AS_JWKS_URI=$(echo "$d" | jq -r .jwks_uri)
  fi
  export LEDGERLINE_AS_TOKEN_URL LEDGERLINE_AS_JWKS_URI
}

# idp_client_auth <name>: how S&V authenticates to an upstream: with its keys
# (private_key_jwt) unless the upstream was given a client secret in .env
idp_client_auth() { if [ -n "$(_idp_var "$1" CLIENT_SECRET)" ]; then echo client_secret_post; else echo private_key_jwt; fi; }

# xaa_env: S&V's client key at Ledgerline's AS
xaa_env() {
  realm_signing_key sv-xaa-client
  SV_XAA_CLIENT_CERT=$(pem_body "$LAB_STATE/keys/sv-xaa-client.crt")
  SV_XAA_CLIENT_KID=$(xaa_client_jwks | jq -r '.keys[0].kid')
  export SV_XAA_CLIENT_CERT SV_XAA_CLIENT_KID SV_CLIENT_AT_LEDGERLINE
}

# xaa_secrets: S&V's key for Ledgerline's AS, kept with its egress gateway
xaa_secrets() {
  K create secret generic xaa-client-key -n agentgateway-system --from-file=signingKey="$LAB_STATE/keys/sv-xaa-client.key" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
}

# xaa_gateway_apply: ai-gateway's Cross App Access (xaa/ledgerline.yaml), with
# how it reaches Ledgerline's token endpoint: its Keycloak's Service, or a
# static backend for an AS outside the lab
xaa_gateway_apply() {
  local dir=$1 ep
  ep=$(jq -cn --arg u "$LEDGERLINE_AS_TOKEN_URL" --arg grp "$AGW_BACKEND_GROUP" --arg kind "$AGW_BACKEND_KIND" '
    ($u | capture("^https://(?<host>[^/:]+)(:(?<port>[0-9]+))?(?<path>/.*)?$")) as $c
    | if $c == null then error("not an https URL: \($u)") else . end
    | ("xaa-" + ($c.host | gsub("[.]"; "-"))) as $n
    | {ref: {group: $grp, kind: $kind, name: $n}, host: $c.host, path: $c.path,
       backend: {name: $n, host: $c.host, port: ($c.port // "443" | tonumber)}}') || die "XAA: Ledgerline's token endpoint"
  echo "$ep" | jq -c --arg api "$AGW_BACKEND_API" --arg kind "$AGW_BACKEND_KIND" --arg tld "$LAB_TLD" '.backend
    | {apiVersion: $api, kind: $kind,
       metadata: {name: .name, namespace: "agentgateway-system", labels: {"lab.solo.io/xaa-endpoint": "true"}},
       spec: {static: {host: .host, port: .port},
              policies: {tls: ({sni: .host} + if (.host | endswith(".\($tld)")) then {caCertificateRefs: [{name: "lab-ca-bundle"}]} else {} end)}}}' \
    | K apply -f - >/dev/null || die "XAA: Ledgerline's endpoint backend"
  XAA_AS_REF=$(echo "$ep" | jq -c .ref) XAA_AS_HOST=$(echo "$ep" | jq -r .host) XAA_AS_PATH=$(echo "$ep" | jq -r .path) \
    apply_tmpl "$dir/xaa/ledgerline.yaml" || die "XAA: the route and its checks"
  # an endpoint no longer used
  for n in $(K get "$AGW_BACKEND_KIND" -n agentgateway-system -l lab.solo.io/xaa-endpoint -o name); do
    [ "${n##*/}" = "$(echo "$ep" | jq -r .backend.name)" ] || K delete -n agentgateway-system "$n" >/dev/null
  done
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
# kc_jwks <key> <alg>: a lab key (.lab/keys, made if missing) as Keycloak
# publishes it in a realm's JWKS: kid is the SHA-256 of its public key info
kc_jwks() {
  local kid
  realm_signing_key "$1"
  kid=$(openssl x509 -in "$LAB_STATE/keys/$1.crt" -pubkey -noout | openssl pkey -pubin -outform DER \
    | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')
  jwks_of "$LAB_STATE/keys/$1.crt" | jq --arg kid "$kid" --arg alg "$2" '.keys[0] |= (.kid = $kid | .alg = $alg)'
}
# sv_upstream_client_jwks: S&V's client at an upstream IdP: the broker's key
# (PS256, as S&V's Keycloak publishes it)
sv_upstream_client_jwks() { kc_jwks sv-broker-client PS256; }

# workforce_realm <realm.json>: S&V's workforce IdP, with S&V's client keys
workforce_realm() {
  jq --arg jwks "$(sv_upstream_client_jwks | jq -c .)" \
    '(.clients[] | select(.clientId == "sterling-vance-broker") | .attributes["jwks.string"]) = $jwks' "$1"
}

# contingency_realm <realm.json>: S&V's contingency IdP, with the broker's key
contingency_realm() {
  jq --arg jwks "$(sv_upstream_client_jwks | jq -c .)" \
    '(.clients[] | select(.clientId == "sterling-vance-broker") | .attributes["jwks.string"]) = $jwks' "$1"
}

# xaa_client_jwks: S&V's client key at Ledgerline's AS
xaa_client_jwks() { realm_signing_key sv-xaa-client; jwks_of "$LAB_STATE/keys/sv-xaa-client.crt"; }

# ledgerline_realm <realm.json>: Ledgerline's realm, trusting S&V's broker for
# sign-in and for ID-JAGs, for S&V's domain only, and naming S&V's client
# there SV_CLIENT_AT_LEDGERLINE
ledgerline_realm() {
  jq --arg domain "$SV_DOMAIN" --arg cid "$SV_CLIENT_AT_LEDGERLINE" '
    (.identityProviders[] | select(.alias == "sterling-vance") | .config.claimFilterValue) = (".*@" + ($domain | gsub("\\."; "\\.")))
    | (.clients[] | select(.clientId == "sterling-vance-kagent") | .clientId) = $cid' "$1"
}

# ledgerline_research: filters ledgerline/research.yaml for an AS outside the
# lab: the MCP server and its waypoint accept tokens from it
ledgerline_research() {
  if ! ras_external; then cat; return; fi
  yq '
    (select(.kind == "MCPServer") | .spec.deployment.env) |= (
      .RESEARCH_ISSUER = strenv(LEDGERLINE_AS_ISSUER) | .RESEARCH_JWKS_URL = strenv(LEDGERLINE_AS_JWKS_URI))
    | (select(.metadata.name == "research-callers") | .spec.traffic.jwtAuthentication.providers[0]) |= (
      .issuer = strenv(LEDGERLINE_AS_ISSUER) | .jwks.remote = {"url": strenv(LEDGERLINE_AS_JWKS_URI), "cacheDuration": "5m"})'
}

# ledgerline_egress_hosts: internet hosts Ledgerline's own services reach: its
# AS's keys when that AS is outside the lab (the MCP server verifies its
# tokens). The lab's own hosts are reached through the edge.
ledgerline_egress_hosts() {
  if ras_external; then echo "$LEDGERLINE_AS_JWKS_URI" | sed -E 's#^https://([^/:]+).*#\1#'; fi
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
    | jq -r --arg keep " $(echo $hosts) " '.items[] | select(.spec.hosts[0] as $h | $keep | contains(" " + $h + " ") | not) | .metadata.name' \
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

# a malformed ENTERPRISE_IDP stops whatever sources this, before it changes anything
idp_names >/dev/null
