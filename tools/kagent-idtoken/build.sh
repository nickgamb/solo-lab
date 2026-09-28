#!/usr/bin/env bash
# Build kagent's controller v0.10.2 + patches/ (ID token forwarding), exactly as
# upstream `make build-controller` does: same Dockerfile, same version ldflags,
# and the runtime-image digests of the RELEASED 0.10.2 images (released-digests.env,
# verified against the released controller binary), so the patched controller
# starts agents on precisely the images a stock 0.10.2 would.
set -euo pipefail
cd "$(dirname "$0")"
TAG=${TAG:-0.10.2-idtoken.1}
IMG=${IMG:-localhost:5001/kagent-dev/kagent/controller:$TAG}
SRC=${SRC:-../../.lab/cache/kagent-build}
rm -rf "$SRC"; git clone -q --depth 1 --branch v0.10.2 https://github.com/kagent-dev/kagent.git "$SRC"
git -C "$SRC" -c user.name=lab -c user.email=lab@solo.lab am -q "$PWD"/patches/*.patch
set -a; . ./released-digests.env; set +a
DIG=$(cd "$SRC" && bash scripts/controller-digest-ldflags.sh)
V=github.com/kagent-dev/kagent/go/core/internal/version
LDF="-X $V.Version=v$TAG -X $V.GitCommit=$(git -C "$SRC" rev-parse --short HEAD) -X $V.BuildDate=$(date -u +%Y-%m-%d)$DIG"
docker build -q --build-arg LDFLAGS="$LDF" --build-arg BUILD_PACKAGE=core/cmd/controller/main.go \
  -t "$IMG" -f "$SRC/go/Dockerfile" "$SRC/go"
docker push -q "$IMG"
echo "built $IMG"
