# shellcheck shell=bash
# Shared helpers. Source from any script:  . "$(dirname "$0")/../scripts/lib.sh"
set -euo pipefail

LAB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAB_STATE="$LAB_ROOT/.lab"          # gitignored: generated files, caches
mkdir -p "$LAB_STATE/cache"

# --- config layering ---------------------------------------------------------
# Highest first:
# 1. the caller's environment (the shell, `make llm LLM_PROVIDER=anthropic`)
# 2. .env               secrets + personal overrides (gitignored)
# 3. config/lab.env     lab shape
# 4. config/oss.env     OSS pins
# 5. config/enterprise.env  ENT_* pins, promoted per product whose
#                       <PRODUCT>_EDITION=enterprise
# .env and the caller are applied before the config files (lab.env derives
# names from them) and again after (so they override the pins too).
_lab_caller=$(export -p | grep -v -E '^declare -x (SHELLOPTS|BASHOPTS)=' || true)
set -a
[ -f "$LAB_ROOT/.env" ] && . "$LAB_ROOT/.env"
eval "$_lab_caller"
. "$LAB_ROOT/config/lab.env"
. "$LAB_ROOT/config/oss.env"
. "$LAB_ROOT/config/enterprise.env"
[ -f "$LAB_ROOT/.env" ] && . "$LAB_ROOT/.env"
eval "$_lab_caller"
set +a
unset _lab_caller

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

# Docker Desktop's credsStore can wedge helm's OCI pulls for minutes. Every
# chart we pull is public,
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

# --- host portability (macOS, Linux, WSL) ----------------------------------
# sha256 / sha1: coreutils where present, else shasum (macOS). Same output.
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }
sha1()   { if command -v sha1sum >/dev/null; then sha1sum "$@"; else shasum "$@"; fi; }
new_uuid() { python3 -c 'import uuid; print(uuid.uuid4())'; }
# docker_desktop: Docker Desktop (macOS, Windows/WSL, or Linux), whose engine
# runs in a VM, as opposed to Docker Engine on this host.
docker_desktop() { docker info --format '{{.OperatingSystem}}' 2>/dev/null | grep -q 'Docker Desktop'; }

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
# mesh up front; ones helm makes with --create-namespace are not.
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
# apply_kustomize <dir>: a kustomization, with the lab registry port and image
# tags filled in (kustomize itself doesn't substitute variables)
apply_kustomize() { K kustomize "$1" | envsubst '${LAB_REGISTRY_PORT} ${SUBSTRATE_LAB_TAG} ${KAGENT_LAB_TAG}' | kagent_edition | K apply -f - >/dev/null; }
# kagent_edition: agent manifests as the running kagent edition can serve them.
# kagent-enterprise passes agents the user's access token, not
# the ID token, so Cross App Access (which trades Bob's ID token for an ID-JAG)
# can't work through chat there, and an MCP server that refuses the agent fails
# its whole turn: the Ledgerline tool is left out.
kagent_edition() {
  if [ "${KAGENT_EDITION:-oss}" = enterprise ]; then
    yq 'del(select(.kind == "SandboxAgent") | .spec.declarative.tools[] | select(.mcpServer.name == "ledgerline-research"))'
  else cat; fi
}

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

# in_registry <repo:tag>: is the image in the lab registry?
in_registry() {
  curl -sfI -o /dev/null "http://localhost:$LAB_REGISTRY_PORT/v2/${1%:*}/manifests/${1##*:}" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
}

# lab_build <repo> <context> [dockerfile]: an image tagged by a hash of its
# build context (and Dockerfile), built and pushed to the lab registry unless
# it's already there. Prints the image. A changed source is a new tag, so a
# rollout; an unchanged one is never rebuilt.
lab_build() {
  local df=${3:-$2/Dockerfile} tag img
  tag=$( { (cd "$2" && find . -type f -not -name .DS_Store | LC_ALL=C sort | xargs cat); cat "$df"; } | sha1 | cut -c1-12)
  img="localhost:$LAB_REGISTRY_PORT/$1:$tag"
  if ! in_registry "$1:$tag"; then
    docker build -q -t "$img" -f "$df" "$2" >/dev/null && docker push -q "$img" >/dev/null || die "building $img failed"
  fi
  echo "$img"
}

# lab_image <repo:tag> <build.sh>: build a patched image (tools/*) into the lab
# registry unless it's already there. The registry outlives clusters.
lab_image() { in_registry "$1" || { step "Building $1 ($2)"; bash "$LAB_ROOT/$2"; }; }

# deny_internet <ns>...: the namespace's pods reach the cluster and nothing
# else. Open: pods, Services, the addresses Istio gives external ServiceEntries
# (so a call to an upstream IdP still leaves through S&V's egress gateway), and
# the API server. Everything else, the internet included, is dropped. The ways
# out of the lab are the gateways built for it (ai-gateway, the egress waypoint),
# whose namespaces are not passed here.
deny_internet() {
  local pods svcs api
  pods=$(K -n kube-system get cm kubeadm-config -o jsonpath='{.data.ClusterConfiguration}' | awk '/podSubnet/{print $2}')
  svcs=$(K -n kube-system get cm kubeadm-config -o jsonpath='{.data.ClusterConfiguration}' | awk '/serviceSubnet/{print $2}')
  api=$(K get endpointslice -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{.items[0].endpoints[0].addresses[0]}')
  local ns
  for ns in "$@"; do
    K apply -f - >/dev/null <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: no-internet, namespace: $ns}
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
  - to:
    - ipBlock: {cidr: ${pods:-10.244.0.0/16}}
    - ipBlock: {cidr: ${svcs:-10.96.0.0/16}}
    - ipBlock: {cidr: 240.240.0.0/16}      # Istio's ServiceEntry addresses (egress waypoint)
  - to: [{ipBlock: {cidr: $api/32}}]
    ports: [{port: 6443, protocol: TCP}]
YAML
  done
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
  export KC_REALM; KC_REALM=$(jq -r .realm "$realm")
  export KC_REALM_SHA; KC_REALM_SHA=$(sha256 "$realm" | cut -c1-16)
  export KC_KEY_SHA; KC_KEY_SHA=$(sha256 "$LAB_STATE/keys/$rname.crt" | cut -c1-16)
  K create configmap "$KC_REALM_CM" -n "$ns" --from-file="$(basename "$realm")=$realm" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
  apply_tmpl "$LAB_ROOT/platform/45-identity/keycloak.yaml"
  K rollout status deploy/keycloak -n "$ns" --timeout=300s >/dev/null
}

# on_exit <command>: run <command> when the (sub)shell that registered it
# exits. Commands stack (the last registered runs first), so helpers and
# scripts never replace each other's cleanup the way a second `trap ... EXIT`
# would. A $(...) subshell inherits the list, so each entry carries the shell
# that registered it and only that shell runs it.
# ($(exec sh -c 'echo $PPID') is this shell's pid: bash 3.2, macOS's, has no BASHPID.)
_LAB_ON_EXIT=()
on_exit() { _LAB_ON_EXIT+=("$(exec sh -c 'echo $PPID') $1"); trap _lab_on_exit EXIT; }
_lab_on_exit() {
  local i e me; me=$(exec sh -c 'echo $PPID')
  for ((i = ${#_LAB_ON_EXIT[@]} - 1; i >= 0; i--)); do
    e=${_LAB_ON_EXIT[i]}
    if [ "${e%% *}" = "$me" ]; then eval "${e#* }" || true; fi
  done
  return 0
}

# free_port: an unused local TCP port (for port-forwards).
free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }

# port_forward <ns> <svc> <localport> <remoteport>: background port-forward on
# 127.0.0.1, torn down when the calling script exits.
port_forward() {
  K port-forward -n "$1" "svc/$2" "$3:$4" >/dev/null 2>&1 &
  local pid=$!; on_exit "kill $pid 2>/dev/null"
  wait_for "port-forward $1/$2" 20 0.5 curl -s -o /dev/null "http://127.0.0.1:$3/"
}

# need_password_grant: the scripted checks sign in with the password grant.
need_password_grant() {
  [ "$LAB_PASSWORD_GRANT" = true ] || die "these checks sign in with the password grant, which is off (LAB_PASSWORD_GRANT=$LAB_PASSWORD_GRANT in config/lab.env)"
}

# kc_token <ns> <realm> <client> <secret> <user> <pass>: a password grant for
# the scripted checks, over a localhost port-forward to the party's Keycloak
# (the issuer claim is still https://idp.<party>.lab). Prints the token
# response (access_token, id_token). The request body goes on stdin, so the
# client secret and password never sit on a command line.
kc_token() {
  need_password_grant
  local lp; lp=$(free_port)
  port_forward "$1" keycloak "$lp" 80
  _C=$3 _S=$4 _U=$5 _P=$6 jq -rn \
    '{grant_type: "password", client_id: $ENV._C, username: $ENV._U, password: $ENV._P, scope: "openid"}
     + (if $ENV._S == "" then {} else {client_secret: $ENV._S} end) | to_entries | map("\(.key)=\(.value | @uri)") | join("&")' \
    | curl -s "http://127.0.0.1:$lp/realms/$2/protocol/openid-connect/token" --data @-
}
# user_token: kc_token's access token alone
user_token() { kc_token "$@" | jq -r .access_token; }

# a2a_send <access token> <id token> <json-rpc body>: one A2A turn with Bob's
# agent, sent as kagent's UI would (from probe-kagent-ui, with the edge's
# Authorization and IdToken cookie). The tokens reach the pod on stdin.
a2a_send() {
  printf '%s\n%s\n%s\n' "$1" "$2" "$3" | K exec -i -n kagent probe-kagent-ui -- sh -c \
    'read -r t; read -r i; read -r b; curl -s -m 300 http://kagent-controller.kagent:8083/api/a2a-sandboxes/sv-agents/bob-assistant/ \
       -H "authorization: Bearer $t" -H "cookie: IdToken=$i" -H "content-type: application/json" -d "$b"'
}
# with_bearer <token> curl <args...>: curl with "Authorization: Bearer <token>"
# read from stdin (-H @-), so the token isn't on curl's command line
with_bearer() { local t=$1; shift; "$@" -H @- <<<"authorization: Bearer $t"; }

# probe_exec <ns>/<pod> <mcp-probe args...>: run tools/mcp-probe.py in a probe
# pod. A --token value, and --header values for authorization and
# x-id-token, travel on stdin instead of the command line.
probe_exec() {
  local ns=${1%%/*} pod=probe out=() in=() h
  [[ $1 == */* ]] && pod=${1#*/}; shift
  while [ $# -gt 0 ]; do
    case "$1" in
      --token) out+=(--token -); in+=("$2"); shift 2 ;;
      --header)
        h=${2%%=*}
        case "$(echo "$h" | tr A-Z a-z)" in
          authorization|x-id-token) out+=(--header "$h=-"); in+=("${2#*=}") ;;
          *) out+=(--header "$2") ;;
        esac; shift 2 ;;
      *) out+=("$1"); shift ;;
    esac
  done
  { [ ${#in[@]} -eq 0 ] || printf '%s\n' "${in[@]}"; } | K exec -i -n "$ns" "$pod" -- python3 /tmp/p.py ${out[@]+"${out[@]}"}
}

# probe_pod <ns> [sa]: a toolbox pod with tools/mcp-probe.py, in <ns>. With no
# <sa> it is pod "probe" with its own ServiceAccount "probe" (so its own SPIFFE
# identity: some workload, nobody special). With <sa> it is pod "probe-<sa>"
# running as that existing ServiceAccount, to act as that workload (e.g. an
# agent's worker pool, or kagent's UI) in a check; it is deleted when the
# script exits, so nothing is left holding that identity.
probe_pod() {
  local ns=$1 sa=${2:-probe} pod=probe
  [ "$sa" = probe ] || { pod=probe-$sa; on_exit "K delete pod $pod -n $ns --wait=false >/dev/null 2>&1"; }
  # pod specs are immutable: replace a probe that predates the current spec,
  # and never reuse one that is already going away (an earlier run's cleanup)
  local img cur; img=$(lab_build lab/toolbox "$LAB_ROOT/tools/toolbox")
  cur=$(K get pod "$pod" -n "$ns" -o jsonpath='{.spec.containers[0].image}|{.metadata.deletionTimestamp}' 2>/dev/null || true)
  if [ -n "$cur" ] && [ "$cur" != "$img|" ]; then
    K delete pod "$pod" -n "$ns" --now --ignore-not-found >/dev/null 2>&1 || true
    K wait --for=delete "pod/$pod" -n "$ns" --timeout=60s >/dev/null 2>&1 || true
  fi
  [ "$sa" = probe ] && K create serviceaccount probe -n "$ns" --dry-run=client -o yaml | K apply -f - >/dev/null
  K apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $pod, namespace: $ns, labels: {app: probe}}
spec:
  serviceAccountName: $sa
  containers:
  - name: probe
    image: $img
    env: [{name: SSL_CERT_FILE, value: /etc/lab-ca/ca.crt}]     # trust-manager bundle, like real workloads
    volumeMounts: [{name: lab-ca, mountPath: /etc/lab-ca, readOnly: true}]
  volumes: [{name: lab-ca, configMap: {name: lab-ca-bundle}}]
YAML
  K wait --for=condition=Ready "pod/$pod" -n "$ns" --timeout=120s >/dev/null
  # as an argument, not stdin: `kubectl exec -i` sometimes delivers an empty stdin
  K exec -n "$ns" "$pod" -- sh -c 'echo "$1" | base64 -d > /tmp/p.py' _ "$(base64 < "$LAB_ROOT/tools/mcp-probe.py" | tr -d '\n')"
}
