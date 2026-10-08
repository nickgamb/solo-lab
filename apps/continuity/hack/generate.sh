#!/usr/bin/env bash
# Regenerate the API's deepcopy functions and CRDs (config/crd) from
# api/v1alpha1 with controller-gen, in the same Go image the controller builds
# with. Run after changing a type; commit what it writes.
set -euo pipefail
APP="$(cd "$(dirname "$0")/.." && pwd)"
ROOT="$(cd "$APP/../.." && pwd)"
GOIMG=golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414
mkdir -p "$ROOT/.lab/cache/gomod" "$ROOT/.lab/cache/gobuild"
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOPATH=/tmp/go -e GOMODCACHE=/go/pkg/mod -e GOFLAGS=-mod=mod \
  -v "$APP:/src" -v "$ROOT/.lab/cache/gomod:/go/pkg/mod" -v "$ROOT/.lab/cache/gobuild:/tmp/.cache" -w /src "$GOIMG" \
  go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0 \
    object paths=./api/... \
    crd paths=./api/... output:crd:artifacts:config=config/crd
