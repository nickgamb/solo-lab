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
CRDS=$KGATEWAY_CRDS_CHART
if [ "$ED" = enterprise ]; then
  # Enterprise kgateway 2.3.5's EnterpriseKgatewayTrafficPolicy CRD is over
  # Kubernetes 1.37's CEL cost budget, and the API server refuses it. Drop the
  # rules it names (input format checks: durations, sizes, rate-limit entries)
  # from a local copy of the chart.
  CRDS="$LAB_STATE/cache/enterprise-kgateway-crds-$KGATEWAY_VERSION"
  if [ ! -d "$CRDS" ]; then
    rm -rf "$CRDS.tmp"
    H pull "$KGATEWAY_CRDS_CHART" --version "$KGATEWAY_VERSION" --untar --untardir "$CRDS.tmp" >"$LAB_STATE/helm-kgateway-crds-pull.log" 2>&1 \
      || die "pulling $KGATEWAY_CRDS_CHART $KGATEWAY_VERSION failed (log: .lab/helm-kgateway-crds-pull.log)"
    mv "$CRDS.tmp/enterprise-kgateway-crds" "$CRDS"; rm -rf "$CRDS.tmp"
    p=.spec.versions[].schema.openAPIV3Schema.properties.spec.properties
    yq -i "del($p.entJWT.properties[].properties.providers.additionalProperties.properties.jwks.properties.remote.properties.cacheDuration.\"x-kubernetes-validations\")
      | del($p.rateLimit.properties.global.properties.descriptors.items.properties.entries.items.\"x-kubernetes-validations\")
      | del($p.buffer.properties.maxRequestSize.\"x-kubernetes-validations\")" \
      "$CRDS/templates/enterprisekgateway.solo.io_enterprisekgatewaytrafficpolicies.yaml"
  fi
fi
helm_up "$KGATEWAY_RELEASE-crds" "$CRDS" "$([ "$CRDS" = "$KGATEWAY_CRDS_CHART" ] && echo "$KGATEWAY_VERSION")" kgateway-system
values_for "$D" values "$ED"
helm_up "$KGATEWAY_RELEASE" "$KGATEWAY_CHART" "$KGATEWAY_VERSION" kgateway-system ${VALS[@]+"${VALS[@]}"}

step "Party edge certificates"
apply_tmpl "$D/party-tls.yaml"
for ns in kgateway-system sv-identity alice meridian ledgerline-identity; do
  K wait -n "$ns" certificate/edge-tls --for=condition=Ready --timeout=120s >/dev/null
done
ok "issued by lab-ca; each party holds its own key"

step "Edge gateway (kgateway-system/edge)"
apply_tmpl "$D/edge-gateway.yaml" "$D/redirect.yaml"
wait_for "edge gateway Programmed" 60 3 \
  K wait -n kgateway-system gateway/edge --for=condition=Programmed --timeout=2s
# The edge takes traffic in; everything it forwards to is in the cluster.
deny_internet kgateway-system
ok "edge programmed: https://*.${OPS_DOMAIN} *.${SV_DOMAIN} *.${ALICE_DOMAIN} *.${MERIDIAN_DOMAIN} *.${LEDGERLINE_DOMAIN}"
