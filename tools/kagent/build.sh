#!/usr/bin/env bash
# Build kagent v0.10.2 + patches/ exactly as upstream `make build-controller`
# does: same Dockerfile, same version ldflags, and the runtime-image digests of
# the RELEASED 0.10.2 images (released-digests.env, verified against the
# released controller binary) for every runtime image the patches don't touch.
#   golang-adk       built from the patched source (0002), pushed here
#   golang-adk-full  the released index, mirrored here by digest (one registry
#                    serves both Go variants: controller.goAgentImage.registry)
#   controller       built from the patched source (0001, 0003), digests baked in
set -euo pipefail
cd "$(dirname "$0")"
TAG=${TAG:-${KAGENT_LAB_TAG:-0.10.2-lab.3}}
REG=${REG:-localhost:${LAB_REGISTRY_PORT:-5001}}
IMG=${IMG:-$REG/kagent-dev/kagent/controller:$TAG}
ADK=$REG/kagent-dev/kagent/golang-adk
SRC=${SRC:-../../.lab/cache/kagent-build}
rm -rf "$SRC"; git -c advice.detachedHead=false clone -q --depth 1 --branch v0.10.2 https://github.com/kagent-dev/kagent.git "$SRC"
git -C "$SRC" -c user.name=lab -c user.email=lab@solo.lab am -q "$PWD"/patches/*.patch
set -a; . ./released-digests.env; set +a
V=github.com/kagent-dev/kagent/go/core/internal/version
LDF="-X $V.Version=v$TAG -X $V.GitCommit=$(git -C "$SRC" rev-parse --short HEAD) -X $V.BuildDate=$(date -u +%Y-%m-%d)"

docker build -q --build-arg LDFLAGS="$LDF" --build-arg BUILD_PACKAGE=adk/cmd/main.go \
  -t "$ADK:$TAG" -f "$SRC/go/Dockerfile" "$SRC/go" >/dev/null
docker push -q "$ADK:$TAG" >/dev/null
docker buildx imagetools create -t "$ADK:$TAG-full" "$GOLANG_ADK_FULL_IMG" >/dev/null 2>&1
GOLANG_ADK_IMG=$ADK:$TAG GOLANG_ADK_FULL_IMG=$ADK@${GOLANG_ADK_FULL_IMG##*@}
echo "built $ADK:$TAG (+ $TAG-full mirrored)"

DIG=$(cd "$SRC" && GOLANG_ADK_IMG=$GOLANG_ADK_IMG GOLANG_ADK_FULL_IMG=$GOLANG_ADK_FULL_IMG bash scripts/controller-digest-ldflags.sh)
docker build -q --build-arg LDFLAGS="$LDF$DIG" --build-arg BUILD_PACKAGE=core/cmd/controller/main.go \
  -t "$IMG" -f "$SRC/go/Dockerfile" "$SRC/go" >/dev/null
docker push -q "$IMG" >/dev/null
echo "built $IMG"
