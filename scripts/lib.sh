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
for _p in ISTIO KGATEWAY AGW KAGENT AGENTREGISTRY; do
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
export SOLO_AGENTREGISTRY_LICENSE_KEY="${SOLO_AGENTREGISTRY_LICENSE_KEY:-${SOLO_LICENSE_KEY:-}}"

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
else _LB='' _LG='' _LY='' _LR='' _LC='' _LN=''; fi
step() { printf '\n%s==> %s%s\n' "$_LB$_LC" "$*" "$_LN"; }
ok()   { printf '%s  ✓ %s%s\n' "$_LG" "$*" "$_LN"; }
warn() { printf '%s  ! %s%s\n' "$_LY" "$*" "$_LN" >&2; }
die()  { printf '%s  ✗ %s%s\n' "$_LR" "$*" "$_LN" >&2; exit 1; }

# --- host portability (macOS, Linux, WSL) ----------------------------------
# sha256 / sha1: coreutils where present, else shasum (macOS). Same output.
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }
# shellcheck disable=SC2120  # callers pipe into it
sha1()   { if command -v sha1sum >/dev/null; then sha1sum "$@"; else shasum "$@"; fi; }
new_uuid() { python3 -c 'import uuid; print(uuid.uuid4())'; }
# docker_desktop: Docker Desktop (macOS, Windows/WSL, or Linux), whose engine
# runs in a VM, as opposed to Docker Engine on this host.
docker_desktop() { docker info --format '{{.OperatingSystem}}' 2>/dev/null | grep -q 'Docker Desktop'; }
# docker_engine: desktop | orbstack | colima | engine (Docker Engine on this
# Linux host) | unknown. On macOS every engine runs in a Linux VM.
docker_engine() {
  local os name; os=$(docker info --format '{{.OperatingSystem}}' 2>/dev/null || true)
  name=$(docker info --format '{{.Name}}' 2>/dev/null || true)
  case "$os" in
    *"Docker Desktop"*) echo desktop ;;
    *OrbStack*) echo orbstack ;;
    *) if [ "$name" = colima ] || [ "$(docker context show 2>/dev/null || true)" = colima ]; then echo colima
       elif [ "$(uname -s)" = Linux ]; then echo engine
       else echo unknown; fi ;;
  esac
}
# docker_vm: the engine runs in a VM (its kernel settings are the VM's, and the
# host is host.docker.internal), not on this host
docker_vm() { case "$(docker_engine)" in desktop|orbstack|colima) return 0 ;; esac; return 1; }

# --- kube / helm ------------------------------------------------------------
K() { kubectl --context "$KCTX" "$@"; }
H() {
  if [ "${LAB_USE_DOCKER_CREDS:-0}" = 1 ]; then helm --kube-context "$KCTX" "$@"
  else DOCKER_CONFIG="$HELM_DOCKER_CONFIG" helm --kube-context "$KCTX" "$@"; fi
}

# helm_unstick <release> <namespace>: a release left pending by an
# interrupted helm (another operation is in progress, it says, forever): a
# first install is uninstalled, an upgrade or rollback rolled back.
helm_unstick() {
  local st; st=$(H status "$1" -n "$2" -o json 2>/dev/null | jq -r '.info.status // empty' || true)
  case "$st" in
    pending-install)
      warn "helm $1 is stuck in pending-install: uninstalling it to install again"
      H uninstall "$1" -n "$2" --wait >>"$LAB_STATE/helm-$1.log" 2>&1 || die "helm uninstall $1 failed (log: .lab/helm-$1.log)" ;;
    pending-upgrade|pending-rollback)
      warn "helm $1 is stuck in $st: rolling back to its last release"
      H rollback "$1" -n "$2" --wait >>"$LAB_STATE/helm-$1.log" 2>&1 || die "helm rollback $1 failed (log: .lab/helm-$1.log)" ;;
  esac
}

# helm_up <release> <chart> <version> <namespace> [helm args...]
# upgrade --install with --wait; retries once (OCI registries flake).
helm_up() {
  local rel=$1 chart=$2 ver=$3 ns=$4; shift 4
  local args=(upgrade --install "$rel" "$chart" --namespace "$ns" --create-namespace --wait --timeout "${HELM_TIMEOUT:-10m}")
  [ -n "$ver" ] && args+=(--version "$ver")
  : >"$LAB_STATE/helm-$rel.log"
  helm_unstick "$rel" "$ns"
  if ! H "${args[@]}" "$@" >>"$LAB_STATE/helm-$rel.log" 2>&1; then
    warn "helm $rel failed once, retrying (log: .lab/helm-$rel.log)"
    sleep 5
    helm_unstick "$rel" "$ns"
    H "${args[@]}" "$@" >>"$LAB_STATE/helm-$rel.log" 2>&1 || { tail -20 "$LAB_STATE/helm-$rel.log" >&2; die "helm $rel failed"; }
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
apply_kustomize() { K kustomize "$1" | envsubst '${LAB_REGISTRY_PORT} ${SUBSTRATE_LAB_TAG} ${KAGENT_LAB_TAG}' | K apply -f - >/dev/null; }

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
# Rendered values can hold secrets (license keys, passwords): owner-only files.
values_for() {  # values_for <dir> <name> <edition>
  local dir=$1 name=$2 ed=$3 f out um
  VALS=()
  um=$(umask); umask 077
  mkdir -p "$LAB_STATE/values"; chmod 700 "$LAB_STATE/values"
  for f in "$dir/$name.yaml" "$dir/$name-$ed.yaml"; do
    [ -f "$f" ] || continue
    out="$LAB_STATE/values/$(basename "$dir")-$(basename "$f")"
    rm -f "$out"   # a file from before keeps its mode when overwritten
    render "$f" > "$out" || { umask "$um"; die "rendering $f failed"; }
    VALS+=(-f "$out")
  done
  umask "$um"
}

# in_registry <repo:tag>: is the image in the lab registry?
in_registry() {
  curl -sfI -o /dev/null "http://localhost:$LAB_REGISTRY_PORT/v2/${1%:*}/manifests/${1##*:}" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
}

# src_files <dir>: the files `docker build <dir>` sends, NUL-separated, sorted,
# relative to <dir>: what git tracks or would track (never what it ignores),
# less <dir>/.dockerignore's patterns. Outside a git checkout, every file.
src_files() {
  ( cd "$1" && if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
      git ls-files -z --cached --others --exclude-standard -- .
    else find . -type f -print0; fi ) | python3 -c '
import os, re, sys
ctx = sys.argv[1]
def rx(p):  # a .dockerignore pattern: path.Match per segment, ** any depth
    out, i = "", 0
    while i < len(p):
        c = p[i]
        if p.startswith("**", i):
            i += 2
            if p.startswith("/", i): i += 1; out += "(?:.*/)?"
            else: out += ".*"
            continue
        if c == "*": out += "[^/]*"
        elif c == "?": out += "[^/]"
        elif c == "[":
            j = p.find("]", i)
            if j < 0: out += re.escape(c)
            else: out += "[" + p[i + 1:j].replace("\\", "\\\\") + "]"; i = j
        elif c == "\\" and i + 1 < len(p): i += 1; out += re.escape(p[i])
        else: out += re.escape(c)
        i += 1
    return re.compile("^" + out + "(?:/.*)?$")   # a match on a directory takes its contents
rules = []
try:
    for line in open(os.path.join(ctx, ".dockerignore")):
        line = line.strip()
        if not line or line.startswith("#"): continue
        neg = line.startswith("!")
        pat = os.path.normpath(line[1:] if neg else line).lstrip("/")
        rules.append((neg, rx(pat)))
except FileNotFoundError:
    pass
keep = set()
for f in sys.stdin.buffer.read().split(b"\0"):
    if not f: continue
    p = os.path.normpath(f.decode("utf-8", "surrogateescape"))
    if not os.path.isfile(os.path.join(ctx, p)) or os.path.basename(p) == ".DS_Store": continue
    out = False
    for neg, r in rules:
        if r.match(p): out = not neg
    if not out: keep.add(p)
sys.stdout.buffer.write(b"".join(k.encode("utf-8", "surrogateescape") + b"\0" for k in sorted(keep)))
' "$1"
}

# src_hash <dir> [file...]: 12 hex chars over a build context's files (names
# and contents, as src_files lists them) and any files outside it (a
# Dockerfile kept elsewhere). The tag of an image built from <dir>.
src_hash() {
  local d=$1 f; shift
  { src_files "$d" | (cd "$d" && xargs -0 openssl dgst -sha1)
    for f in "$@"; do cat "$f"; done; } | sha1 | cut -c1-12
}

# lab_build <repo> <context> [dockerfile]: an image tagged by a hash of its
# build context (and Dockerfile), built and pushed to the lab registry unless
# it's already there. Prints the image. A changed source is a new tag, so a
# rollout; an unchanged one is never rebuilt.
lab_build() {
  local df=${3:-$2/Dockerfile} tag img
  tag=$(src_hash "$2" "$df") || die "hashing $2 failed"
  img="localhost:$LAB_REGISTRY_PORT/$1:$tag"
  if ! in_registry "$1:$tag"; then
    docker build -q -t "$img" -f "$df" "$2" >/dev/null && docker push -q "$img" >/dev/null || die "building $img failed"
  fi
  echo "$img"
}

# lab_image <repo:tag> <build.sh>: build a patched image (tools/*) into the lab
# registry unless it's already there. The registry outlives clusters. A
# failed build fails the caller.
lab_image() {
  in_registry "$1" && return 0
  step "Building $1 ($2)"
  bash "$LAB_ROOT/$2" || die "$2 failed: $1 is not in the lab registry"
  in_registry "$1" || die "$2 finished without pushing $1"
}

# kc_idjag_image: S&V's patched Keycloak (tools/keycloak-idjag), tagged by its
# version and a hash of its sources, so a changed patch is a new image
kc_idjag_image() {
  echo "lab/keycloak-idjag:${KEYCLOAK_VERSION}-pr49998-$(src_hash "$LAB_ROOT/tools/keycloak-idjag")"
}

# build_src <name>: an empty scratch directory for a tools/*/build.sh checkout,
# <name> under LAB_BUILD_SRC (default .lab/cache). Prints it. Anything not
# under .lab is refused: it is removed first.
build_src() {
  local parent=${LAB_BUILD_SRC:-$LAB_STATE/cache} d state
  d=$(python3 -c 'import os, sys; print(os.path.realpath(os.path.join(sys.argv[1], sys.argv[2])))' "$parent" "$1")
  state=$(python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$LAB_STATE")
  case "$d" in "$state"/?*) ;; *) die "LAB_BUILD_SRC=$parent: $d is not under $LAB_STATE, refusing to remove it" ;; esac
  rm -rf "$d"; mkdir -p "$d"
  echo "$d"
}

# fetch <url> <file> [check...]: download atomically. curl fails on an HTTP
# error; <check> (a command, given the downloaded file as its last argument)
# must pass before the file takes <file>'s place. A partial or bad download
# never lands.
fetch() {
  local url=$1 dst=$2 tmp; shift 2
  tmp=$(mktemp "$dst.XXXXXX")
  if ! curl -fsSL --retry 3 --connect-timeout 15 -o "$tmp" "$url"; then rm -f "$tmp"; die "download failed: $url"; fi
  [ -s "$tmp" ] || { rm -f "$tmp"; die "empty download: $url"; }
  if [ $# -gt 0 ] && ! "$@" "$tmp"; then rm -f "$tmp"; die "download did not verify: $url"; fi
  mv -f "$tmp" "$dst"
}

# deny_internet <ns>...: the namespace's pods reach the cluster and nothing
# else. Open: pods, Services, the addresses Istio gives external ServiceEntries
# (so a call to an upstream IdP still leaves through S&V's egress gateway), and
# the API server. Everything else, the internet included, is dropped. The ways
# out of the lab are the gateways built for it (ai-gateway, the egress waypoint),
# whose namespaces are not passed here.
deny_internet() {
  local ns
  for ns in "$@"; do _no_internet "$ns" no-internet '{}'; done
}
# deny_internet_pods <ns> <app>...: the same, for the pods labelled app=<app>
# only, in a namespace whose other pods do reach the internet (a gateway)
deny_internet_pods() {
  local ns=$1 app; shift
  for app in "$@"; do _no_internet "$ns" "no-internet-$app" "{matchLabels: {app: $app}}"; done
}
_no_internet() {  # _no_internet <ns> <policy name> <podSelector>
  local pods svcs api cfg
  cfg=$(K -n kube-system get cm kubeadm-config -o jsonpath='{.data.ClusterConfiguration}')
  pods=$(echo "$cfg" | awk '/podSubnet/{print $2}')
  svcs=$(echo "$cfg" | awk '/serviceSubnet/{print $2}')
  api=$(K get endpointslice -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{.items[0].endpoints[0].addresses[0]}')
  [ -n "$api" ] || die "no API server address (endpointslice default/kubernetes)"
  K apply -f - >/dev/null <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: $2, namespace: $1}
spec:
  podSelector: $3
  policyTypes: [Egress]
  egress:
  - to:
    - ipBlock: {cidr: ${pods:-10.244.0.0/16}}
    - ipBlock: {cidr: ${svcs:-10.96.0.0/16}}
    - ipBlock: {cidr: 240.240.0.0/16}      # Istio's ServiceEntry addresses (egress waypoint)
  - to: [{ipBlock: {cidr: $api/32}}]
    ports: [{port: 6443, protocol: TCP}]
YAML
}

need_cluster() { K get --raw /readyz >/dev/null 2>&1 || die "cluster $KCTX is not reachable — run: make cluster"; }

# lab_hosts: every hostname the lab publishes, <name>.<party domain>, as the
# manifests, config and scripts name them (a hosts file, the preflight check)
lab_hosts() {
  local re='[a-z0-9-]+\.\$\{?[A-Z_]+_DOMAIN\}?'
  { git -C "$LAB_ROOT" grep -ohE "$re" -- '*.yaml' '*.json' '*.env' '*.sh' 2>/dev/null \
      || grep -rhoE --include='*.yaml' --include='*.json' --include='*.env' --include='*.sh' "$re" \
           "$LAB_ROOT/config" "$LAB_ROOT/platform" "$LAB_ROOT/demos" "$LAB_ROOT/scripts"
  } | grep -v PARTY_DOMAIN | envsubst | sort -u | tr '\n' ' ' | sed 's/ $//'
}

# ollama_host_url: Ollama as this host reaches it. OLLAMA_URL is how the
# cluster reaches it; host.docker.internal or the kind gateway is this host.
ollama_host_url() {
  local u=${OLLAMA_URL:-http://localhost:11434} h port
  h=${u#*://}; h=${h%%/*}; port=${h##*:}; [ "$port" = "$h" ] && port=11434
  case "${h%%:*}" in
    host.docker.internal|host.lima.internal|host.orb.internal) echo "http://localhost:$port"; return ;;
  esac
  if docker network inspect kind --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' 2>/dev/null | tr ' ' '\n' | grep -qxF "${h%%:*}"; then
    echo "http://localhost:$port"
  else
    echo "${u%/}"
  fi
}

# llm_ready: the LLM the lab is pointed at (make llm) answers with its model.
# Prints why not and fails. Lists models only: no tokens spent.
llm_ready() {
  local p m
  p=$(K get ns agentgateway-system -o jsonpath='{.metadata.annotations.lab\.solo\.io/llm-provider}' 2>/dev/null || true)
  m=$(K get ns agentgateway-system -o jsonpath='{.metadata.annotations.lab\.solo\.io/llm-model}' 2>/dev/null || true)
  case "$p" in
    ollama)
      curl -sf -m 5 "$(ollama_host_url)/api/tags" | jq -e --arg m "$m" '.models[] | select(.name == $m)' >/dev/null 2>&1 \
        || { echo "ollama at $(ollama_host_url) is not answering with model $m"; return 1; } ;;
    openai)
      [ -n "${OPENAI_API_KEY:-}" ] || { echo "no OPENAI_API_KEY"; return 1; }
      curl -sf -m 10 -o /dev/null https://api.openai.com/v1/models -H @- <<<"authorization: Bearer $OPENAI_API_KEY" \
        || { echo "api.openai.com is not answering for this key"; return 1; } ;;
    anthropic)
      [ -n "${ANTHROPIC_API_KEY:-}" ] || { echo "no ANTHROPIC_API_KEY"; return 1; }
      curl -sf -m 10 -o /dev/null https://api.anthropic.com/v1/models -H 'anthropic-version: 2023-06-01' -H @- <<<"x-api-key: $ANTHROPIC_API_KEY" \
        || { echo "api.anthropic.com is not answering for this key"; return 1; } ;;
    *) echo "no LLM configured (make llm)"; return 1 ;;
  esac
}

# lab_secret <NAME>: a random secret generated once and kept in .lab/secrets.env
# (gitignored), so reinstalls and restarts agree on client secrets.
lab_secret() {
  local f="$LAB_STATE/secrets.env" v
  touch "$f"; chmod 600 "$f"
  v=$(grep -E "^$1=" "$f" | cut -d= -f2- || true)
  if [ -z "$v" ]; then v=$(openssl rand -hex 24); echo "$1=$v" >> "$f"; fi
  printf '%s' "$v"
}
# lab_secret_get <NAME>: an existing .lab/secrets.env value, for a $(...) or a
# pipe; never generated (its layer does that), never printed to a terminal
lab_secret_get() {
  local v
  [ -t 1 ] && die "lab_secret_get $1: not printing a secret to a terminal"
  v=$(grep -E "^$1=" "$LAB_STATE/secrets.env" 2>/dev/null | head -1 | cut -d= -f2- || true)
  [ -n "$v" ] || die "no $1 in .lab/secrets.env: install its layer first"
  printf '%s' "$v"
}

# secret_apply <ns> <name> <key>=<value>...: a generic Secret, created or
# updated. The values go to kubectl as files in a private temp dir (umask
# 077), never on a command line, where any process list would show them.
secret_apply() {
  local ns=$1 name=$2 kv k d um args=(); shift 2
  um=$(umask); umask 077
  d=$(mktemp -d)
  for kv in "$@"; do
    k=${kv%%=*}
    printf '%s' "${kv#*=}" >"$d/$k"
    args+=(--from-file="$k=$d/$k")
  done
  umask "$um"
  if ! K create secret generic "$name" -n "$ns" ${args[@]+"${args[@]}"} --dry-run=client -o yaml | K apply -f - >/dev/null; then
    rm -rf "$d"; die "secret $ns/$name: apply failed"
  fi
  rm -rf "$d"
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
# signing key comes from kc-realm-key (stable across restarts). Published at
# https://idp.<domain>, or KC_HOST.<domain>.
deploy_keycloak() {
  local ns=$1 domain=$2 listener=$3 realm=$4 features=${5:-token-exchange-standard}
  export KC_IMAGE="${KC_IMAGE:-quay.io/keycloak/keycloak:$KEYCLOAK_VERSION}"
  local rname; rname=$(basename "$realm" .json | sed 's/^realm-//')
  realm_signing_key "$rname"
  secret_apply "$ns" kc-realm-key \
    KC_REALM_RSA_KEY="$(pem_body "$LAB_STATE/keys/$rname.key")" \
    KC_REALM_RSA_CERT="$(pem_body "$LAB_STATE/keys/$rname.crt")"
  export PARTY_NS=$ns PARTY_DOMAIN=$domain PARTY_LISTENER=$listener KC_FEATURES=$features PARTY_HOST=${KC_HOST:-idp}
  export KC_REALM_CM; KC_REALM_CM="realm-$(basename "$realm" .json | sed 's/^realm-//')"
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
# Ctrl-C and kill run them too (as an exit with the signal's status).
_LAB_ON_EXIT=()
on_exit() {
  _LAB_ON_EXIT+=("$(exec sh -c 'echo $PPID') $1")
  trap _lab_on_exit EXIT; trap 'exit 130' INT; trap 'exit 143' TERM; trap 'exit 129' HUP
}
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
  # kubectl itself, not through K: a backgrounded shell function would be a
  # subshell holding the caller's stdout open (a $(...) or pipe never ends)
  kubectl --context "$KCTX" port-forward -n "$1" "svc/$2" "$3:$4" >/dev/null 2>&1 &
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
user_token() { kc_token "$@" | jq -r '.access_token // empty'; }

# browser_signin <start url> <callback prefix> <user> <pass>: a browser
# sign-in from a script. Follows redirects from <start url> across the lab's
# IdPs, fills the login form of S&V's workforce IdP (login.<sv domain>) once,
# and prints the URL it lands on under <callback prefix> (the app's callback,
# with its code). A sign-in page anywhere else (an IdP on the internet, an
# unexpected step) stops it with an error naming the page.
browser_signin() {
  local url=$1 cb=$2 jar body hdr loc posted="" i action
  jar=$(mktemp) body=$(mktemp); on_exit "rm -f $jar $body"
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16; do
    case "$url" in "$cb"*) echo "$url"; return 0 ;; esac
    case "$url" in
      https://idp."$SV_DOMAIN"/*|https://login."$SV_DOMAIN"/*|https://idp."$LEDGERLINE_DOMAIN"/*) ;;
      *) echo "browser_signin: sign-in continues at ${url%%\?*}, outside S&V's own IdP" >&2; return 1 ;;
    esac
    hdr=$(curl -s --cacert "$LAB_CA_DIR/ca.crt" -b "$jar" -c "$jar" -D - -o "$body" "$url")
    loc=$(echo "$hdr" | awk 'tolower($1)=="location:"{print $2}' | tr -d '\r')
    if [ -z "$loc" ] && [ -z "$posted" ] && grep -q 'id="kc-form-login"' "$body" && case "$url" in https://login."$SV_DOMAIN"/*) true ;; *) false ;; esac; then
      action=$(grep -o 'id="kc-form-login"[^>]*action="[^"]*"' "$body" | sed 's/.*action="//; s/"$//; s/&amp;/\&/g')
      [ -n "$action" ] || action=$(grep -o 'action="[^"]*"' "$body" | head -1 | sed 's/action="//; s/"$//; s/&amp;/\&/g')
      hdr=$(_U=$3 _P=$4 jq -rn '{username: $ENV._U, password: $ENV._P, credentialId: ""} | to_entries | map("\(.key)=\(.value | @uri)") | join("&")' \
        | curl -s --cacert "$LAB_CA_DIR/ca.crt" -b "$jar" -c "$jar" -D - -o "$body" "$action" --data @-)
      loc=$(echo "$hdr" | awk 'tolower($1)=="location:"{print $2}' | tr -d '\r'); posted=1
    fi
    if [ -z "$loc" ]; then
      echo "browser_signin: stopped at ${url%%\?*}: $(echo "$hdr" | head -1 | tr -d '\r'), page: $(grep -o '<title>[^<]*' "$body" | head -1 | sed 's/<title>//') $(grep -o 'kc-feedback-text">[^<]*' "$body" | head -1 | sed 's/.*">//')" >&2
      return 1
    fi
    case "$loc" in /*) loc="$(echo "$url" | sed -E 's#^(https://[^/]+).*#\1#')$loc" ;; esac
    url=$loc
  done
  echo "browser_signin: too many redirects" >&2; return 1
}

# sso_token <user> <pass>: an S&V employee signs in to kagent as in the
# browser (the edge's SSO client, authorization code with PKCE) through the
# broker's active IdP, which must be S&V's own (keycloak). Prints the tokens.
sso_token() {
  local verifier challenge state cb code
  verifier=$(openssl rand -hex 32) state=$(openssl rand -hex 8)
  challenge=$(printf '%s' "$verifier" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')
  cb="https://kagent.$SV_DOMAIN/oauth2/redirect"
  cb=$(browser_signin "https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/auth?client_id=kagent&response_type=code&scope=openid&redirect_uri=$(jq -rn --arg u "$cb" '$u|@uri')&state=$state&code_challenge=$challenge&code_challenge_method=S256" \
    "$cb" "$1" "$2") || return 1
  code=$(echo "$cb" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p')
  [ -n "$code" ] || { echo "sso_token: no code in $cb" >&2; return 1; }
  _C=$code _V=$verifier _S=$(lab_secret_get SV_KAGENT_CLIENT_SECRET) _R="https://kagent.$SV_DOMAIN/oauth2/redirect" jq -rn \
    '{grant_type: "authorization_code", client_id: "kagent", client_secret: $ENV._S, code: $ENV._C, code_verifier: $ENV._V, redirect_uri: $ENV._R}
     | to_entries | map("\(.key)=\(.value | @uri)") | join("&")' \
    | curl -s --cacert "$LAB_CA_DIR/ca.crt" "https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/token" --data @-
}

# a2a_send <access token> <json-rpc body>: one A2A turn with Bob's agent, sent
# as kagent's UI would (from probe-kagent-ui, with the access token the edge
# forwards). The token and body reach the pod on stdin.
a2a_send() {
  printf '%s\n%s\n' "$1" "$2" | K exec -i -n kagent probe-kagent-ui -- sh -c \
    'read -r t; read -r b; curl -s -m 300 http://kagent-controller.kagent:8083/api/a2a-sandboxes/sv-agents/bob-assistant/ \
       -H "authorization: Bearer $t" -H "content-type: application/json" -d "$b"'
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
