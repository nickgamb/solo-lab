#!/usr/bin/env bash
# Istio ambient: base -> istiod -> cni -> ztunnel.
#   oss:        upstream 1.31.1 (charts from the verified release tarball)
#   enterprise: Solo Enterprise for Istio 1.31.1-solo (license on istiod)
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
ED=$ISTIO_EDITION

chart() {  # chart <base|istiod|cni|ztunnel>
  if [ "$ED" = enterprise ]; then echo "$ISTIO_CHART_REPO/$1"; return; fi
  local root="$LAB_STATE/cache/istio-$ISTIO_VERSION/manifests/charts"
  if [ ! -d "$root" ]; then
    local tgz="$LAB_STATE/cache/istio-$ISTIO_VERSION.tgz" arch; arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
    local os; os=$([ "$(uname -s)" = Darwin ] && echo osx || echo linux)
    local url="https://github.com/istio/istio/releases/download/$ISTIO_VERSION/istio-$ISTIO_VERSION-$os-$arch.tar.gz"
    [ -s "$tgz" ] || curl -fsSL -o "$tgz" "$url"
    (cd "$LAB_STATE/cache" && curl -fsSL "$url.sha256" | awk -v f="istio-$ISTIO_VERSION.tgz" '{print $1"  "f}' | shasum -a 256 -c >/dev/null) \
      || die "istio tarball checksum mismatch"
    tar xzf "$tgz" -C "$LAB_STATE/cache" "istio-$ISTIO_VERSION/manifests/charts"
  fi
  case $1 in
    base) echo "$root/base" ;; istiod) echo "$root/istio-control/istio-discovery" ;;
    cni) echo "$root/istio-cni" ;; ztunnel) echo "$root/ztunnel" ;;
  esac
}
ver() { [ "$ED" = enterprise ] && echo "$ISTIO_VERSION" || echo ""; }

if [ "$ED" = enterprise ] && [ -z "$SOLO_ISTIO_LICENSE_KEY" ]; then
  warn "ISTIO_EDITION=enterprise without a license key: install works, peering/enterprise features will not"
fi

step "Istio $ISTIO_VERSION ambient ($ED)"
K create namespace istio-system --dry-run=client -o yaml | K apply -f - >/dev/null
K label namespace istio-system topology.istio.io/network="$MESH_NETWORK" --overwrite >/dev/null

for c in base istiod cni ztunnel; do
  values_for "$D" "$c" "$ED"
  rel=$c; [ "$c" = base ] && rel=istio-base; [ "$c" = cni ] && rel=istio-cni
  helm_up "$rel" "$(chart "$c")" "$(ver)" istio-system ${VALS[@]+"${VALS[@]}"}
done
rollout istio-system deploy/istiod ds/istio-cni-node ds/ztunnel

K apply -f "$D/telemetry.yaml" >/dev/null
ok "mesh-wide access logs + tracing to the OTel collector"
