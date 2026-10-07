#!/usr/bin/env bash
# Tooling + Docker checks. Safe to run any time. macOS, Linux or WSL2.
. "$(dirname "$0")/lib.sh"

# how to install things here, for the hints below
if command -v brew >/dev/null; then pkg="brew install"
elif command -v apt-get >/dev/null; then pkg="sudo apt-get install"
elif command -v dnf >/dev/null; then pkg="sudo dnf install"
else pkg="install"; fi

missing=()
for b in docker kind kubectl helm jq yq envsubst openssl python3 git curl; do command -v "$b" >/dev/null || missing+=("$b"); done
[ ${#missing[@]} -eq 0 ] || die "missing tools: ${missing[*]}  ($pkg ${missing[*]/envsubst/gettext}; kind, kubectl, helm and yq from their releases where the package is old)"
kind version | grep -qE 'v0\.(3[2-9]|[4-9][0-9])' || die "kind >= v0.32 required for k8s 1.37 node images (https://kind.sigs.k8s.io/docs/user/quick-start/#installation)"
yq --version 2>&1 | grep -q mikefarah || die "yq must be mikefarah/yq v4 (https://github.com/mikefarah/yq), not the Python yq wrapper"
docker info >/dev/null 2>&1 || die "Docker is not running (Docker Desktop, OrbStack or Colima: start it; Docker Engine: sudo systemctl start docker)"
# On macOS the engine runs in a Linux VM: Docker Desktop, OrbStack and Colima
# are supported (each forwards 127.0.0.1 ports to the host and names it
# host.docker.internal). Anything else is refused rather than half working.
engine=$(docker_engine)
case "$(uname -s)/$engine" in
  Darwin/desktop|Darwin/orbstack|Darwin/colima|Linux/*) ;;
  Darwin/*) die "unsupported Docker engine on macOS ($(docker info --format '{{.OperatingSystem}}' 2>/dev/null)): use Docker Desktop, OrbStack or Colima" ;;
esac
docker buildx version >/dev/null 2>&1 || die "docker buildx is missing (Docker Engine: $pkg docker-buildx-plugin)"
# ed25519 keys (story 2) need OpenSSL 3; macOS's /usr/bin/openssl is LibreSSL
openssl version | grep -q '^OpenSSL [3-9]' || die "OpenSSL 3 or newer is needed, found: $(openssl version) (brew install openssl, and put it first on PATH)"

mem=$(docker info --format '{{.MemTotal}}'); mem_mb=$((mem / 1024 / 1024)); mem_gb=$(((mem_mb + 512) / 1024))
cpus=$(docker info --format '{{.NCPU}}')
case "$engine" in
  desktop) where="Docker Desktop > Settings > Resources; on WSL2, memory= in %UserProfile%\\.wslconfig" ;;
  orbstack) where="OrbStack > Settings > System > Memory limit" ;;
  colima) where="colima stop && colima start --memory 16 --cpu 8" ;;
  *) where="the memory of this host" ;;
esac
# a VM set to 16 GB reports a little less than 16 GiB; 15 GiB is the floor
if [ "$mem_mb" -lt 15360 ]; then die "Docker has ${mem_gb} GB; the full platform needs >= 16 GB ($where)"; fi
ok "docker: ${mem_gb} GB, ${cpus} CPUs"

# kind + many DaemonSets (ztunnel, istio-cni, node-exporter, atelet) blow
# through the default inotify limits: symptoms are pods stuck in CrashLoop
# with "too many open files". Docker Desktop: raise them inside its VM, where
# they reset when it restarts, so this runs every time (OrbStack and Colima
# likewise: their VM's). Docker Engine: they are this host's own kernel
# settings, so say what to run instead of changing them quietly.
if docker_vm; then
  cur=$(docker run --rm --privileged --pid=host alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 nsenter -t 1 -m -u -n -i sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0)
  if [ "$cur" -lt 1024 ]; then
    docker run --rm --privileged --pid=host alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 nsenter -t 1 -m -u -n -i sh -c \
      'sysctl -qw fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=1048576' \
      && ok "docker VM inotify limits raised" || warn "could not raise inotify limits"
  else
    ok "docker VM inotify limits ok ($cur)"
  fi
else
  cur=$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0)
  if [ "$cur" -lt 1024 ]; then
    die "inotify limits too low ($cur): sudo sysctl -w fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=1048576 (persist in /etc/sysctl.d/99-solo-lab.conf)"
  fi
  ok "inotify limits ok ($cur)"
fi

# Host ports the lab binds on 127.0.0.1: free, or held by the lab itself (a
# re-run). The edge (kind's control-plane node), the local registry, lab DNS.
port_ours() {  # port_ours <port>: a lab container publishes it
  docker ps --filter "publish=$1" --format '{{.Names}}' 2>/dev/null | grep -qE "^(lab-|${LAB_NAME}-control-plane$)"
}
port_busy() { python3 -c 'import socket, sys; s = socket.socket(); s.settimeout(1); sys.exit(0 if s.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)' "$1"; }
for p in LAB_HTTP_PORT:"edge HTTP" LAB_HTTPS_PORT:"edge HTTPS" LAB_REGISTRY_PORT:"local registry" LAB_DNS_PORT:"lab DNS"; do
  v=${p%%:*} what=${p#*:}; n=${!v}
  if port_ours "$n"; then ok "127.0.0.1:$n: the lab's $what"
  elif port_busy "$n"; then die "127.0.0.1:$n ($what) is taken by something else: stop it, or set $v to another port in .env"
  else ok "127.0.0.1:$n free: $what"; fi
done

# The lab's names resolve to 127.0.0.1 on this host (make machine-setup,
# once; lab DNS itself starts with make cluster)
unresolved=""
for h in $(lab_hosts); do
  python3 -c 'import socket, sys; sys.exit(socket.gethostbyname(sys.argv[1]) != "127.0.0.1")' "$h" 2>/dev/null || unresolved="$unresolved $h"
done
if [ -z "$unresolved" ]; then ok "every lab hostname resolves to 127.0.0.1 ($(lab_hosts | wc -w | tr -d ' '))"
else warn "not resolving to 127.0.0.1:$unresolved (make machine-setup, once; then make cluster starts lab DNS)"; fi

# The model: Ollama as this host reaches it (the cluster reaches the same one
# at OLLAMA_URL, or host.docker.internal / the kind gateway)
if [ "${LLM_PROVIDER:-ollama}" = ollama ]; then
  if [ -n "${OLLAMA_URL:-}" ]; then
    echo "$OLLAMA_URL" | grep -qE '^https?://[^/:]+(:[0-9]+)?/?$' || die "OLLAMA_URL must be http(s)://host[:port], not '$OLLAMA_URL'"
  fi
  u=$(ollama_host_url)
  if ! curl -sf -m 3 "$u/api/version" >/dev/null; then
    warn "ollama not reachable at $u (brew services start ollama, or sudo systemctl start ollama): kagent will have no model, and the checks that need one are skipped"
  elif ! curl -sf -m 3 "$u/api/tags" | jq -e --arg m "${OLLAMA_MODEL:-qwen3.8:27b}" '.models[] | select(.name == $m)' >/dev/null; then
    warn "ollama at $u has no ${OLLAMA_MODEL:-qwen3.8:27b} (ollama pull ${OLLAMA_MODEL:-qwen3.8:27b})"
  else ok "ollama at $u with ${OLLAMA_MODEL:-qwen3.8:27b}"; fi
fi
