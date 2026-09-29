#!/usr/bin/env bash
# Build Keycloak $KEYCLOAK_VERSION + PR #49998 (patches/) and push it to the lab
# registry. The first build compiles two Keycloak modules with Maven (several
# minutes); the Maven cache is a BuildKit cache mount, so rebuilds are quick.
set -euo pipefail
cd "$(dirname "$0")"
KEYCLOAK_VERSION=${KEYCLOAK_VERSION:?set KEYCLOAK_VERSION (config/oss.env)}
REG=${REG:-localhost:${LAB_REGISTRY_PORT:-5001}}
IMG=${IMG:-$REG/lab/keycloak-idjag:$KEYCLOAK_VERSION-pr49998}
DOCKER_BUILDKIT=1 docker build -q --build-arg KEYCLOAK_VERSION="$KEYCLOAK_VERSION" -t "$IMG" . >/dev/null
docker push -q "$IMG" >/dev/null
echo "built $IMG"
