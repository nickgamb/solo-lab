#!/usr/bin/env bash
# Create the kind cluster, the registry caches, and cloud-provider-kind.
# Idempotent: re-running skips what exists.
. "$(dirname "$0")/lib.sh"

# upstream registry -> container name. Caches persist in docker volumes, so a
# `make down && make up` re-pulls ~3 GB from local disk instead of the internet.
MIRRORS="
docker.io=https://registry-1.docker.io
ghcr.io=https://ghcr.io
quay.io=https://quay.io
registry.k8s.io=https://registry.k8s.io
gcr.io=https://gcr.io
us-docker.pkg.dev=https://us-docker.pkg.dev
cr.kgateway.dev=https://cr.kgateway.dev
cr.agentgateway.dev=https://cr.agentgateway.dev
"
mirror_name() { echo "lab-mirror-$(echo "$1" | tr '.' '-')"; }

step "Preflight"
"$LAB_ROOT/scripts/preflight.sh"

step "containerd registry config (.lab/certs.d)"
rm -rf "$LAB_STATE/certs.d"; mkdir -p "$LAB_STATE/certs.d"
for m in $MIRRORS; do
  host=${m%%=*}; up=${m#*=}; name=$(mirror_name "$host")
  mkdir -p "$LAB_STATE/certs.d/$host"
  cat > "$LAB_STATE/certs.d/$host/hosts.toml" <<EOF
server = "$up"
[host."http://$name:5000"]
  capabilities = ["pull", "resolve"]
EOF
done
mkdir -p "$LAB_STATE/certs.d/localhost:$LAB_REGISTRY_PORT"
cat > "$LAB_STATE/certs.d/localhost:$LAB_REGISTRY_PORT/hosts.toml" <<EOF
[host."http://lab-registry:5000"]
EOF
ok "$(echo $MIRRORS | wc -w | tr -d ' ') mirrors + localhost:$LAB_REGISTRY_PORT"

step "kind cluster $LAB_NAME ($KIND_NODE_IMAGE, 1 cp + $KIND_WORKERS workers)"
if kind get clusters 2>/dev/null | grep -qx "$LAB_NAME"; then
  ok "exists"
else
  zones=(zone-a zone-b zone-c)
  KIND_WORKER_NODES=""
  for ((i = 0; i < KIND_WORKERS; i++)); do
    KIND_WORKER_NODES+="- role: worker
  image: ${KIND_NODE_IMAGE}
  labels:
    topology.kubernetes.io/region: lab
    topology.kubernetes.io/zone: ${zones[$((i % 3))]}
  extraMounts:
  - hostPath: ${LAB_STATE}/certs.d
    containerPath: /etc/containerd/certs.d
    readOnly: true
"
  done
  export KIND_WORKER_NODES LAB_STATE
  render "$LAB_ROOT/cluster/kind.yaml.tmpl" > "$LAB_STATE/kind.yaml"
  kind create cluster --config "$LAB_STATE/kind.yaml" --wait 120s
  ok "created"
fi
kubectl config use-context "$KCTX" >/dev/null

step "Registry caches (docker network: kind)"
for m in $MIRRORS; do
  host=${m%%=*}; up=${m#*=}; name=$(mirror_name "$host")
  if [ -z "$(docker ps -aq -f name="^${name}$")" ]; then
    docker run -d --restart=always --name "$name" --network kind \
      -v "$name:/var/lib/registry" \
      -e REGISTRY_PROXY_REMOTEURL="$up" \
      -e REGISTRY_STORAGE_DELETE_ENABLED=true \
      -e OTEL_TRACES_EXPORTER=none \
      registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8 >/dev/null
  else
    docker start "$name" >/dev/null
    docker network connect kind "$name" 2>/dev/null || true
  fi
done
if [ -z "$(docker ps -aq -f name='^lab-registry$')" ]; then
  docker run -d --restart=always --name lab-registry --network kind \
    -p "127.0.0.1:$LAB_REGISTRY_PORT:5000" -v lab-registry:/var/lib/registry \
    -e OTEL_TRACES_EXPORTER=none registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8 >/dev/null
else
  docker start lab-registry >/dev/null; docker network connect kind lab-registry 2>/dev/null || true
fi
# KEP-1755: advertise the local registry to tooling (tilt, skaffold, ko...)
K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: local-registry-hosting, namespace: kube-public}
data:
  localRegistryHosting.v1: |
    host: "localhost:$LAB_REGISTRY_PORT"
    hostFromContainerRuntime: "lab-registry:5000"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF
ok "caches up; push your own images to localhost:$LAB_REGISTRY_PORT"

step "cloud-provider-kind (LoadBalancer IPs on the kind network)"
if [ -z "$(docker ps -aq -f name='^lab-cloud-provider-kind$')" ]; then
  docker run -d --restart=always --name lab-cloud-provider-kind --network kind \
    -v "${LAB_DOCKER_SOCK:-/var/run/docker.sock}:/var/run/docker.sock" \
    "registry.k8s.io/cloud-provider-kind/cloud-controller-manager:$CLOUD_PROVIDER_KIND_VERSION" >/dev/null
else
  docker start lab-cloud-provider-kind >/dev/null
fi
ok "running"

step "Cluster ready"
K get nodes -L topology.kubernetes.io/zone
