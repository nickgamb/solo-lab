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
# Kiali runs in its own meshed namespace. A lab from before that has it in
# istio-system (outside the mesh): remove that install and its edge route.
if H status kiali -n istio-system >/dev/null 2>&1; then
  H uninstall kiali -n istio-system --wait >/dev/null
  K delete httproutes.gateway.networking.k8s.io,trafficpolicies.gateway.kgateway.dev,gatewayextensions.gateway.kgateway.dev -n istio-system kiali kiali-sso --ignore-not-found >/dev/null 2>&1 || true
  K delete networkpolicy kiali-callers -n istio-system --ignore-not-found >/dev/null
  K delete secret kiali-sso -n istio-system --ignore-not-found >/dev/null
fi
values_for "$D" kiali oss
helm_up kiali kiali-server "$KIALI_VERSION" kiali --repo https://kiali.org/helm-charts ${VALS[@]+"${VALS[@]}"}

step "Mesh policy"
K apply -f "$D/mesh-policy.yaml" >/dev/null
ok "collector, Prometheus, Tempo, kube-state-metrics and Kiali take only their named callers"

step "Egress: the cluster only"
# Like every party namespace (scripts/lib.sh deny_internet). Prometheus also
# scrapes node-exporter and the kubelets on the node addresses.
deny_internet observability kiali
{ cat <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: prometheus-scrapes-nodes, namespace: observability}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: prometheus}}
  policyTypes: [Egress]
  egress:
  - ports: [{port: 9100, protocol: TCP}, {port: 10250, protocol: TCP}]
    to:
YAML
  K get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' \
    | sed 's|.*|    - ipBlock: {cidr: &/32}|'
} | K apply -f - >/dev/null
ok "observability, kiali: no internet (Prometheus: the nodes' metrics ports too)"

step "Mesh scrape targets"
K apply -f "$D/monitors.yaml" >/dev/null
ok "istiod, ztunnel, waypoint/gateway proxies"

# Grafana and Kiali are published on the edge with their sign-in, by
# platform/90-observatory (platform-uis.yaml), once the admins' IdP exists.
K label namespace istio-system lab.solo.io/party=platform --overwrite >/dev/null
