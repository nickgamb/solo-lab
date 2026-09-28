#!/usr/bin/env bash
# kgateway (Envoy) — the north-south edge. Every UI and API the lab exposes to
# your laptop enters here: https://<name>.<party>.lab -> 127.0.0.1:443 ->
# NodePort 30443 -> edge. In-cluster, CoreDNS sends *.lab to the same edge.
#   oss:        kgateway            (GatewayClass kgateway)
#   enterprise: Solo Enterprise for kgateway (GatewayClass enterprise-kgateway)
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
ED=$KGATEWAY_EDITION

step "kgateway $KGATEWAY_VERSION ($ED)"
helm_up "$KGATEWAY_RELEASE-crds" "$KGATEWAY_CRDS_CHART" "$KGATEWAY_VERSION" kgateway-system
values_for "$D" values "$ED"
helm_up "$KGATEWAY_RELEASE" "$KGATEWAY_CHART" "$KGATEWAY_VERSION" kgateway-system ${VALS[@]+"${VALS[@]}"}

step "Party edge certificates"
apply_tmpl "$D/party-tls.yaml"
for ns in kgateway-system sv-identity alice meridian; do
  K wait -n "$ns" certificate/edge-tls --for=condition=Ready --timeout=120s >/dev/null
done
ok "issued by lab-ca; each party holds its own key"

step "Edge gateway (kgateway-system/edge)"
apply_tmpl "$D/edge-gateway.yaml" "$D/redirect.yaml"
wait_for "edge gateway Programmed" 60 3 \
  K wait -n kgateway-system gateway/edge --for=condition=Programmed --timeout=2s
ok "edge programmed: https://*.${OPS_DOMAIN} *.${SV_DOMAIN} *.${ALICE_DOMAIN} *.${MERIDIAN_DOMAIN}"
