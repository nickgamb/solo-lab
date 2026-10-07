#!/usr/bin/env bash
# Build Keycloak $KEYCLOAK_VERSION + PR #49998 (patches/) and push it to the lab
# registry. The first build compiles two Keycloak modules with Maven (several
# minutes); the Maven cache is a BuildKit cache mount, so rebuilds are quick.
# Tagged by version and a hash of this directory's sources (scripts/lib.sh
# kc_idjag_image), so a changed patch is a new image.
. "$(dirname "$0")/../../scripts/lib.sh"
cd "$(dirname "$0")" || exit 1
REG=${REG:-localhost:${LAB_REGISTRY_PORT:-5001}}
IMG=${IMG:-$REG/$(kc_idjag_image)}
DOCKER_BUILDKIT=1 docker build -q --build-arg KEYCLOAK_VERSION="$KEYCLOAK_VERSION" -t "$IMG" . >/dev/null \
  || die "building $IMG failed"
docker push -q "$IMG" >/dev/null || die "pushing $IMG failed"
echo "built $IMG"
