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

# Each party's certificate names: a wildcard from the lab CA; from Let's
# Encrypt, every hostname the lab publishes under the party's domain
# (lab_hosts), one HTTP-01 challenge each
tls_names() {  # tls_names <domain>
  if [ "$LAB_CERT_ISSUER" = letsencrypt ]; then
    lab_hosts | tr ' ' '\n' | awk -v d=".$1" 'substr($0, length($0) - length(d) + 1) == d {printf "%s\"%s\"", (n++ ? ", " : "["), $0} END {print "]"}'
  else
    echo "[\"*.$1\"]"
  fi
}
export OPS_TLS_NAMES SV_TLS_NAMES ALICE_TLS_NAMES MERIDIAN_TLS_NAMES LEDGERLINE_TLS_NAMES EDGE_HTTP_ROUTES
OPS_TLS_NAMES=$(tls_names "$OPS_DOMAIN")
SV_TLS_NAMES=$(tls_names "$SV_DOMAIN")
ALICE_TLS_NAMES=$(tls_names "$ALICE_DOMAIN")
MERIDIAN_TLS_NAMES=$(tls_names "$MERIDIAN_DOMAIN")
LEDGERLINE_TLS_NAMES=$(tls_names "$LEDGERLINE_DOMAIN")
PARTY_TLS_NS="kgateway-system sv-identity alice meridian ledgerline-identity"
EDGE_HTTP_ROUTES='{from: Same}'

case "$LAB_CERT_ISSUER" in
  lab-ca) ;;
  letsencrypt)
    # port 80 also takes the challenge routes from the certificates' namespaces
    EDGE_HTTP_ROUTES="{from: Selector, selector: {matchExpressions: [{key: kubernetes.io/metadata.name, operator: In, values: [${PARTY_TLS_NS// /, }]}]}}"
    step "Edge gateway (kgateway-system/edge), port 80 for Let's Encrypt"
    apply_tmpl "$D/edge-gateway.yaml" "$D/redirect.yaml" "$D/acme.yaml"
    wait_for "ClusterIssuer letsencrypt Ready" 30 2 K wait clusterissuer/letsencrypt --for=condition=Ready --timeout=2s
    # certificates kept from an earlier build of this domain, still good for
    # 30 days: cert-manager keeps them rather than asking Let's Encrypt again
    CERTS="$LAB_STATE/edge-certs/$LAB_TLD"
    for ns in $PARTY_TLS_NS; do
      f="$CERTS/$ns.yaml"
      [ -s "$f" ] && yq '.data."tls.crt"' "$f" | base64 -d | openssl x509 -noout -checkend 2592000 >/dev/null 2>&1         && K apply -f "$f" >/dev/null
    done
    ;;
  *) die "LAB_CERT_ISSUER=$LAB_CERT_ISSUER: lab-ca or letsencrypt" ;;
esac

step "Party edge certificates"
apply_tmpl "$D/party-tls.yaml"
for ns in $PARTY_TLS_NS; do
  K wait -n "$ns" certificate/edge-tls --for=condition=Ready --timeout="$([ "$LAB_CERT_ISSUER" = letsencrypt ] && echo 300s || echo 120s)" >/dev/null \
    || die "certificate $ns/edge-tls not issued (kubectl describe certificate -n $ns edge-tls; kubectl get challenges -A)"
done
if [ "$LAB_CERT_ISSUER" = letsencrypt ]; then
  mkdir -p "$CERTS" && chmod 700 "$CERTS"
  for ns in $PARTY_TLS_NS; do
    K get secret edge-tls -n "$ns" -o yaml \
      | yq 'del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.ownerReferences, .metadata.managedFields)' \
      > "$CERTS/$ns.yaml" && chmod 600 "$CERTS/$ns.yaml"
  done
fi
ok "issued by $LAB_CERT_ISSUER; each party holds its own key"

step "Edge gateway (kgateway-system/edge)"
apply_tmpl "$D/edge-gateway.yaml" "$D/redirect.yaml"
# Solo Enterprise for kgateway: a web application firewall on the edge
[ "$ED" = enterprise ] && K apply -f "$D/waf.yaml" >/dev/null && ok "WAF on the edge: SQL and script injection refused"
wait_for "edge gateway Programmed" 60 3 \
  K wait -n kgateway-system gateway/edge --for=condition=Programmed --timeout=2s
# The edge takes traffic in; everything it forwards to is in the cluster.
deny_internet kgateway-system
ok "edge programmed: https://*.${OPS_DOMAIN} *.${SV_DOMAIN} *.${ALICE_DOMAIN} *.${MERIDIAN_DOMAIN} *.${LEDGERLINE_DOMAIN}"
