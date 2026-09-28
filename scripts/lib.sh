# shellcheck shell=bash
# Shared helpers. Source from any script:  . "$(dirname "$0")/../scripts/lib.sh"
set -euo pipefail

LAB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAB_STATE="$LAB_ROOT/.lab"          # gitignored: generated files, caches
mkdir -p "$LAB_STATE/cache"

# --- config layering ---------------------------------------------------------
# 1. .env          secrets + personal overrides (gitignored)
# 2. config/lab.env    lab shape
# 3. config/oss.env    OSS pins
# 4. config/enterprise.env  ENT_* pins, promoted per product whose
#                      <PRODUCT>_EDITION=enterprise
set -a
[ -f "$LAB_ROOT/.env" ] && . "$LAB_ROOT/.env"
. "$LAB_ROOT/config/lab.env"
. "$LAB_ROOT/config/oss.env"
. "$LAB_ROOT/config/enterprise.env"
set +a

# Promote ENT_<P>_<X> over <P>_<X> for each product running enterprise.
for _p in ISTIO KGATEWAY AGW KAGENT; do
  _ed="${_p}_EDITION"
  if [ "${!_ed}" = enterprise ]; then
    while IFS='=' read -r _k _; do
      export "${_k#ENT_}=${!_k}"
    done < <(env | grep -E "^ENT_${_p}_" || true)
  fi
done
unset _p _ed _k

# Per-product license keys fall back to one shared key. Solo trial keys are
# often product-scoped (kagent-enterprise rejects an agentgateway key), so
# set the specific one when you have it.
export SOLO_ISTIO_LICENSE_KEY="${SOLO_ISTIO_LICENSE_KEY:-${SOLO_LICENSE_KEY:-}}"
export SOLO_KGATEWAY_LICENSE_KEY="${SOLO_KGATEWAY_LICENSE_KEY:-${SOLO_LICENSE_KEY:-}}"
export SOLO_AGW_LICENSE_KEY="${SOLO_AGW_LICENSE_KEY:-${SOLO_LICENSE_KEY:-}}"
export SOLO_KAGENT_LICENSE_KEY="${SOLO_KAGENT_LICENSE_KEY:-${SOLO_LICENSE_KEY:-}}"

export KCTX="kind-$LAB_NAME"

# Docker Desktop's credsStore can wedge helm's OCI pulls for minutes (hit in
# both uma4agents and kagent-substrate-demo). Every chart we pull is public,
# so give helm an empty docker config. LAB_USE_DOCKER_CREDS=1 opts out.
# Scoped to helm only: the docker CLI needs the real config for its context.
HELM_DOCKER_CONFIG="$LAB_STATE/docker-anon"
mkdir -p "$HELM_DOCKER_CONFIG"; [ -f "$HELM_DOCKER_CONFIG/config.json" ] || echo '{}' > "$HELM_DOCKER_CONFIG/config.json"

# --- output ------------------------------------------------------------------
# Namespaced so scripts sourcing lib.sh can't clobber them.
if [ -t 1 ]; then _LB=$'\e[1m'; _LG=$'\e[32m'; _LY=$'\e[33m'; _LR=$'\e[31m'; _LC=$'\e[36m'; _LN=$'\e[0m'
else _LB= _LG= _LY= _LR= _LC= _LN=; fi
step() { printf '\n%s==> %s%s\n' "$_LB$_LC" "$*" "$_LN"; }
ok()   { printf '%s  ✓ %s%s\n' "$_LG" "$*" "$_LN"; }
warn() { printf '%s  ! %s%s\n' "$_LY" "$*" "$_LN" >&2; }
die()  { printf '%s  ✗ %s%s\n' "$_LR" "$*" "$_LN" >&2; exit 1; }

# --- kube / helm ------------------------------------------------------------
K() { kubectl --context "$KCTX" "$@"; }
H() {
  if [ "${LAB_USE_DOCKER_CREDS:-0}" = 1 ]; then helm --kube-context "$KCTX" "$@"
  else DOCKER_CONFIG="$HELM_DOCKER_CONFIG" helm --kube-context "$KCTX" "$@"; fi
}

# helm_up <release> <chart> <version> <namespace> [helm args...]
# upgrade --install with --wait; retries once (OCI registries flake).
helm_up() {
  local rel=$1 chart=$2 ver=$3 ns=$4; shift 4
  local args=(upgrade --install "$rel" "$chart" --namespace "$ns" --create-namespace --wait --timeout "${HELM_TIMEOUT:-10m}")
  [ -n "$ver" ] && args+=(--version "$ver")
  if ! H "${args[@]}" "$@" >"$LAB_STATE/helm-$rel.log" 2>&1; then
    warn "helm $rel failed once, retrying (log: .lab/helm-$rel.log)"
    sleep 5
    H "${args[@]}" "$@" >"$LAB_STATE/helm-$rel.log" 2>&1 || { tail -20 "$LAB_STATE/helm-$rel.log" >&2; die "helm $rel failed"; }
  fi
  ok "$rel ${ver:-} ($ns)"
}

# ensure_ns <name> [ambient]  — namespaces we create are labelled into the
# mesh up front; helm --create-namespace ones are NOT (uma4agents gotcha).
ensure_ns() {
  K create namespace "$1" --dry-run=client -o yaml | K apply -f - >/dev/null
  [ "${2:-}" = ambient ] && K label namespace "$1" istio.io/dataplane-mode=ambient --overwrite >/dev/null
  return 0
}

# render <file>: substitute only ${UPPER_CASE} vars that appear in the file,
# so CEL, shell and $foo in manifests pass through untouched.
render() {
  local vars; vars=$(grep -oE '\$\{[A-Z][A-Z0-9_]*\}' "$1" | sort -u | tr '\n' ' ' || true)
  envsubst "$vars" < "$1"
}
apply_tmpl() { local f; for f in "$@"; do render "$f" | K apply -f - >/dev/null; done; }

rollout() {  # rollout <ns> <kind/name>...
  local ns=$1; shift; local r
  for r in "$@"; do K rollout status "$r" -n "$ns" --timeout="${ROLLOUT_TIMEOUT:-300s}" >/dev/null; done
}

# wait_for <description> <tries> <sleep> <command...>
wait_for() {
  local what=$1 tries=$2 pause=$3; shift 3; local i
  for ((i = 1; i <= tries; i++)); do "$@" >/dev/null 2>&1 && return 0; sleep "$pause"; done
  die "timed out waiting for $what"
}

# Values layering: sets global VALS=(-f a.yaml -f b.yaml ...) from
#   <dir>/<name>.yaml and <dir>/<name>-<edition>.yaml, each rendered through
#   envsubst into .lab/values/. (bash 3.2: no mapfile, no namerefs.)
values_for() {  # values_for <dir> <name> <edition>
  local dir=$1 name=$2 ed=$3 f out
  VALS=()
  for f in "$dir/$name.yaml" "$dir/$name-$ed.yaml"; do
    [ -f "$f" ] || continue
    out="$LAB_STATE/values/$(basename "$dir")-$(basename "$f")"; mkdir -p "$(dirname "$out")"
    render "$f" > "$out"; VALS+=(-f "$out")
  done
}

# lab_image <repo:tag> <build.sh>: build a patched image (tools/*) into the lab
# registry unless it's already there. The registry outlives clusters.
lab_image() {
  curl -sfI -o /dev/null "http://localhost:$LAB_REGISTRY_PORT/v2/${1%:*}/manifests/${1##*:}" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    || { step "Building $1 ($2)"; bash "$LAB_ROOT/$2"; }
}

need_cluster() { K get --raw /readyz >/dev/null 2>&1 || die "cluster $KCTX is not reachable — run: make cluster"; }

# lab_secret <NAME>: a random secret generated once and kept in .lab/secrets.env
# (gitignored), so reinstalls and restarts agree on client secrets.
lab_secret() {
  local f="$LAB_STATE/secrets.env" v
  touch "$f"; chmod 600 "$f"
  v=$(grep -E "^$1=" "$f" | cut -d= -f2- || true)
  if [ -z "$v" ]; then v=$(openssl rand -hex 24); echo "$1=$v" >> "$f"; fi
  printf '%s' "$v"
}

# realm_signing_key <name>: a stable RS256 key + cert for a realm, generated
# once into .lab/keys. Keycloak's dev store is ephemeral; without a fixed key
# every IdP restart rotates the signing key and breaks every JWKS cache.
realm_signing_key() {
  local d="$LAB_STATE/keys"; mkdir -p "$d"; chmod 700 "$d"
  if [ ! -s "$d/$1.key" ]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=$1" \
      -keyout "$d/$1.key" -out "$d/$1.crt" >/dev/null 2>&1
    chmod 600 "$d/$1.key"
  fi
}
pem_body() { grep -v -- '-----' "$1" | tr -d '\n'; }

# deploy_keycloak <ns> <domain> <listener> <realm.json> [features]
# One Keycloak per party; see platform/45-identity/keycloak.yaml. The realm
# signing key comes from kc-realm-key (stable across restarts).
deploy_keycloak() {
  local ns=$1 domain=$2 listener=$3 realm=$4 features=${5:-token-exchange-standard}
  export KC_IMAGE="${KC_IMAGE:-quay.io/keycloak/keycloak:$KEYCLOAK_VERSION}"
  local rname; rname=$(basename "$realm" .json | sed 's/^realm-//')
  realm_signing_key "$rname"
  K create secret generic kc-realm-key -n "$ns" \
    --from-literal=KC_REALM_RSA_KEY="$(pem_body "$LAB_STATE/keys/$rname.key")" \
    --from-literal=KC_REALM_RSA_CERT="$(pem_body "$LAB_STATE/keys/$rname.crt")" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
  export PARTY_NS=$ns PARTY_DOMAIN=$domain PARTY_LISTENER=$listener KC_FEATURES=$features
  export KC_REALM_CM="realm-$(basename "$realm" .json | sed 's/^realm-//')"
  export KC_REALM_SHA; KC_REALM_SHA=$(shasum -a 256 "$realm" | cut -c1-16)
  export KC_KEY_SHA; KC_KEY_SHA=$(shasum -a 256 "$LAB_STATE/keys/$rname.crt" | cut -c1-16)
  K create configmap "$KC_REALM_CM" -n "$ns" --from-file="$(basename "$realm")=$realm" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
  apply_tmpl "$LAB_ROOT/platform/45-identity/keycloak.yaml"
  K rollout status deploy/keycloak -n "$ns" --timeout=300s >/dev/null
}

# port_forward <ns> <svc> <localport> <remoteport>: background port-forward on
# 127.0.0.1, torn down when the calling script exits.
port_forward() {
  K port-forward -n "$1" "svc/$2" "$3:$4" >/dev/null 2>&1 &
  local pid=$!; trap "kill $pid 2>/dev/null" EXIT
  wait_for "port-forward $1/$2" 20 0.5 curl -s -o /dev/null "http://127.0.0.1:$3/"
}

# user_token <ns> <realm> <client> <secret> <user> <pass>: a password-grant
# access token for scripted checks, fetched over a localhost port-forward to
# the party's Keycloak (the issuer claim is still https://idp.<party>.lab).
user_token() {
  local lp=$((18000 + RANDOM % 1000))
  port_forward "$1" keycloak "$lp" 80
  curl -s "http://127.0.0.1:$lp/realms/$2/protocol/openid-connect/token" \
    -d grant_type=password -d client_id="$3" ${4:+-d client_secret="$4"} \
    -d username="$5" -d password="$6" -d scope=openid | jq -r .access_token
}

# probe_pod <ns>: a toolbox pod with its own ServiceAccount (so its own SPIFFE
# identity) in <ns>, with tools/mcp-probe.py copied in.
probe_pod() {
  # pod specs are immutable: replace a probe that predates the current spec
  K get pod probe -n "$1" -o jsonpath='{.spec.volumes[*].name}' 2>/dev/null | grep -q lab-ca \
    || K delete pod probe -n "$1" --now --ignore-not-found >/dev/null 2>&1
  K apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: probe, namespace: $1}
---
apiVersion: v1
kind: Pod
metadata: {name: probe, namespace: $1, labels: {app: probe}}
spec:
  serviceAccountName: probe
  containers:
  - name: probe
    image: localhost:${LAB_REGISTRY_PORT}/lab/toolbox:1
    env: [{name: SSL_CERT_FILE, value: /etc/lab-ca/ca.crt}]     # trust-manager bundle, like real workloads
    volumeMounts: [{name: lab-ca, mountPath: /etc/lab-ca, readOnly: true}]
  volumes: [{name: lab-ca, configMap: {name: lab-ca-bundle}}]
YAML
  K wait --for=condition=Ready "pod/probe" -n "$1" --timeout=120s >/dev/null
  K exec -i -n "$1" probe -- sh -c 'cat > /tmp/p.py' < "$LAB_ROOT/tools/mcp-probe.py"
}
