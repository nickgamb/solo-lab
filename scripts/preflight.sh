#!/usr/bin/env bash
# Tooling + Docker VM checks. Safe to run any time.
. "$(dirname "$0")/lib.sh"

missing=()
for b in docker kind kubectl helm jq yq envsubst openssl node; do command -v "$b" >/dev/null || missing+=("$b"); done
[ ${#missing[@]} -eq 0 ] || die "missing tools: ${missing[*]}  (brew install ${missing[*]/envsubst/gettext})"
kind version | grep -qE 'v0\.(3[2-9]|[4-9][0-9])' || die "kind >= v0.32 required for k8s 1.37 node images (brew upgrade kind)"
docker info >/dev/null 2>&1 || die "Docker is not running (open -a Docker)"

mem=$(docker info --format '{{.MemTotal}}'); mem_gb=$((mem / 1024 / 1024 / 1024))
cpus=$(docker info --format '{{.NCPU}}')
if [ "$mem_gb" -lt 16 ]; then die "Docker VM has ${mem_gb} GB; the full platform needs >= 16 GB (Docker Desktop > Settings > Resources)"; fi
ok "docker: ${mem_gb} GB, ${cpus} CPUs"

# kind + many DaemonSets (ztunnel, istio-cni, node-exporter, atelet) blow
# through the Docker VM's default inotify limits: symptoms are pods stuck in
# CrashLoop with "too many open files". Raise them inside the VM (not macOS);
# they reset when Docker Desktop restarts, so this runs every time.
cur=$(docker run --rm --privileged --pid=host alpine:3.22 nsenter -t 1 -m -u -n -i sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0)
if [ "$cur" -lt 1024 ]; then
  docker run --rm --privileged --pid=host alpine:3.22 nsenter -t 1 -m -u -n -i sh -c \
    'sysctl -qw fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=1048576' \
    && ok "docker VM inotify limits raised" || warn "could not raise inotify limits"
else
  ok "docker VM inotify limits ok ($cur)"
fi

if [ "${LLM_PROVIDER:-ollama}" = ollama ]; then
  if curl -s -m 3 localhost:11434/api/version >/dev/null; then ok "ollama reachable on :11434"
  else warn "ollama not reachable on :11434 (brew services start ollama) — kagent will have no model"; fi
fi
