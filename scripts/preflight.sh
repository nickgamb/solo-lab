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
docker info >/dev/null 2>&1 || die "Docker is not running (Docker Desktop: start it; Docker Engine: sudo systemctl start docker)"
docker buildx version >/dev/null 2>&1 || die "docker buildx is missing (Docker Engine: $pkg docker-buildx-plugin)"
# ed25519 keys (story 2) need OpenSSL 3; macOS's /usr/bin/openssl is LibreSSL
openssl version | grep -q '^OpenSSL [3-9]' || die "OpenSSL 3 or newer is needed, found: $(openssl version) (brew install openssl, and put it first on PATH)"

mem=$(docker info --format '{{.MemTotal}}'); mem_mb=$((mem / 1024 / 1024)); mem_gb=$(((mem_mb + 512) / 1024))
cpus=$(docker info --format '{{.NCPU}}')
if docker_desktop; then where="Docker Desktop > Settings > Resources; on WSL2, memory= in %UserProfile%\\.wslconfig"
else where="the memory of this host"; fi
# a VM set to 16 GB reports a little less than 16 GiB; 15 GiB is the floor
if [ "$mem_mb" -lt 15360 ]; then die "Docker has ${mem_gb} GB; the full platform needs >= 16 GB ($where)"; fi
ok "docker: ${mem_gb} GB, ${cpus} CPUs"

# kind + many DaemonSets (ztunnel, istio-cni, node-exporter, atelet) blow
# through the default inotify limits: symptoms are pods stuck in CrashLoop
# with "too many open files". Docker Desktop: raise them inside its VM, where
# they reset when it restarts, so this runs every time. Docker Engine: they
# are this host's own kernel settings, so say what to run instead of
# changing them quietly.
if docker_desktop; then
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

if [ "${LLM_PROVIDER:-ollama}" = ollama ]; then
  if curl -s -m 3 localhost:11434/api/version >/dev/null; then ok "ollama reachable on :11434"
  else warn "ollama not reachable on :11434 (brew services start ollama, or sudo systemctl start ollama) — kagent will have no model"; fi
fi
