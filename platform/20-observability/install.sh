#!/usr/bin/env bash
# Observability: one OTLP front door (otel-collector) for every component.
#   traces  -> Tempo (service graphs + span metrics -> Prometheus)
#   metrics -> Prometheus (OTLP receiver + ServiceMonitors/PodMonitors)
#   UI      -> Grafana (dashboards via sidecar, any namespace), Kiali (mesh)
# Installed before the gateways: their charts ship ServiceMonitors.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
NS=observability

step "kube-prometheus-stack $KPS_VERSION"
K create secret generic grafana-admin -n "$NS" --from-literal=admin-user=admin \
  --from-literal=admin-password="$(lab_secret GRAFANA_ADMIN_PASSWORD)" --dry-run=client -o yaml | K apply -f - >/dev/null
values_for "$D" kps oss
HELM_TIMEOUT=15m helm_up kps kube-prometheus-stack "$KPS_VERSION" $NS \
  --repo https://prometheus-community.github.io/helm-charts ${VALS[@]+"${VALS[@]}"}

step "Tempo $TEMPO_VERSION"
values_for "$D" tempo oss
helm_up tempo tempo "$TEMPO_VERSION" $NS --repo https://grafana.github.io/helm-charts ${VALS[@]+"${VALS[@]}"}

step "OpenTelemetry Collector $OTEL_COLLECTOR_VERSION"
values_for "$D" otel-collector oss
helm_up otel-collector opentelemetry-collector "$OTEL_COLLECTOR_VERSION" $NS \
  --repo https://open-telemetry.github.io/opentelemetry-helm-charts ${VALS[@]+"${VALS[@]}"}

step "Kiali $KIALI_VERSION"
values_for "$D" kiali oss
helm_up kiali kiali-server "$KIALI_VERSION" istio-system --repo https://kiali.org/helm-charts ${VALS[@]+"${VALS[@]}"}

step "Mesh scrape targets"
K apply -f "$D/monitors.yaml" >/dev/null
ok "istiod, ztunnel, waypoint/gateway proxies"

# Grafana and Kiali are published on the edge with their sign-in, by
# platform/90-observatory (platform-uis.yaml), once the admins' IdP exists.
K label namespace istio-system lab.solo.io/party=platform --overwrite >/dev/null
