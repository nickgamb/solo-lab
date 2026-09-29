#!/usr/bin/env bash
# Build Agent Substrate v0.0.9 + patches/ into the lab registry under one tag:
# ateapi, atecontroller and ateom-gvisor from source (the way `ko build` does),
# atelet and atenet mirrored from the release by digest (released-digests.env).
set -euo pipefail
cd "$(dirname "$0")"
TAG=${TAG:-${SUBSTRATE_LAB_TAG:-v0.0.9-lab.1}}
REPO=${REPO:-localhost:${LAB_REGISTRY_PORT:-5001}/kagent-dev/substrate}
SRC=${SRC:-../../.lab/cache/substrate-build}
rm -rf "$SRC"; git -c advice.detachedHead=false clone -q --depth 1 --branch v0.0.9 https://github.com/kagent-dev/substrate.git "$SRC"
git -C "$SRC" -c user.name=lab -c user.email=lab@solo.lab am -q "$PWD"/patches/*.patch
build() {
  docker build -q --build-arg VERSION="$TAG" --target "$1" -t "$REPO/$1:$TAG" -f Dockerfile "$SRC" >/dev/null
  docker push -q "$REPO/$1:$TAG" >/dev/null; echo "built $REPO/$1:$TAG"
}
build ateapi; build atecontroller
. ./released-digests.env
for img in "$ATELET_IMG" "$ATENET_IMG"; do
  c=$(basename "${img%@*}"); docker pull -q "$img" >/dev/null
  docker tag "$img" "$REPO/$c:$TAG"; docker push -q "$REPO/$c:$TAG" >/dev/null; echo "mirrored $REPO/$c:$TAG"
done
# last: platform/50-substrate takes this image's presence to mean the whole
# set is in the registry, so a build cut short is redone
build ateom-gvisor
# The patched WorkerPool CRD; platform/50-substrate swaps it into the release chart
cp "$SRC/charts/substrate-crds/templates/ate.dev_workerpools.yaml" crds/
