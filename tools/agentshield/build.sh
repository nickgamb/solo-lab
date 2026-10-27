#!/usr/bin/env bash
# Build Agentshield at AGENTSHIELD_COMMIT + patches/ with its own Dockerfile
# (the web UI built and embedded) into the lab registry as agentshield_image.
. "$(dirname "$0")/../../scripts/lib.sh"
IMG="localhost:$LAB_REGISTRY_PORT/$(agentshield_image)"
SRC=$(agentshield_src)
docker build -q --build-arg VERSION="0.1.0-lab" --build-arg COMMIT="$AGENTSHIELD_COMMIT" \
  -t "$IMG" "$SRC" >/dev/null || die "building $IMG failed"
docker push -q "$IMG" >/dev/null || die "pushing $IMG failed"
echo "built $IMG"
