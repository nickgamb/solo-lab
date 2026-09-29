#!/usr/bin/env bash
# Gateway API CRDs, metrics-server (HPAs), cert-manager, trust-manager and the
# lab CA, the lab namespaces, and CoreDNS answering *.lab with the edge.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster

step "Gateway API CRDs $GATEWAY_API_VERSION ($GATEWAY_API_CHANNEL)"
f="$LAB_STATE/cache/gateway-api-$GATEWAY_API_VERSION-$GATEWAY_API_CHANNEL.yaml"
[ -s "$f" ] || curl -fsSL -o "$f" "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/$GATEWAY_API_CHANNEL-install.yaml"
# server-side: several CRDs exceed the 256KB last-applied annotation limit
K apply --server-side --force-conflicts -f "$f" >/dev/null
ok "applied"

step "metrics-server $METRICS_SERVER_VERSION (HPAs)"
# kind kubelets serve self-signed certs, hence --kubelet-insecure-tls
helm_up metrics-server metrics-server "$METRICS_SERVER_VERSION" kube-system \
  --repo https://kubernetes-sigs.github.io/metrics-server/ \
  --set 'args={--kubelet-insecure-tls}' --set replicas=2

step "cert-manager $CERT_MANAGER_VERSION + lab CA"
helm_up cert-manager cert-manager "$CERT_MANAGER_VERSION" cert-manager \
  --repo https://charts.jetstack.io --set crds.enabled=true --set replicaCount=2 \
  --set podDisruptionBudget.enabled=true
helm_up trust-manager trust-manager "$TRUST_MANAGER_VERSION" cert-manager \
  --repo https://charts.jetstack.io --set app.trust.namespace=cert-manager \
  --set secretTargets.enabled=false
"$LAB_ROOT/scripts/ca.sh"
K create secret tls lab-ca -n cert-manager --cert="$LAB_CA_DIR/ca.crt" --key="$LAB_CA_DIR/ca.key" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
K apply -f "$D/lab-ca.yaml" >/dev/null
wait_for "ClusterIssuer lab-ca Ready" 30 2 K wait clusterissuer/lab-ca --for=condition=Ready --timeout=2s
ok "ClusterIssuer lab-ca + Bundle lab-ca-bundle (all namespaces)"

step "CoreDNS: *.${LAB_TLD} -> edge (split horizon)"
"$D/coredns-patch.sh"
ok "patched"

step "Party namespaces"
K apply -f "$D/namespaces.yaml" >/dev/null
K get ns -L lab.solo.io/party,istio.io/dataplane-mode,istio.io/use-waypoint --no-headers \
  | awk '$4!="" {printf "    %-22s %-16s %-8s %s\n", $1, $4, $5, $6}'
