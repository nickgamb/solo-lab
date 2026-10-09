#!/usr/bin/env bash
# The Observatory: a live view of the whole lab (topology, traffic, identity
# continuity), for platform admins. Its own Keycloak (realm ops, ops-identity) signs admins in
# at the edge; it reads the cluster as itself and changes it only as the
# signed-in admin. Every gateway exports its access log to it through the
# collector. Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
APP="$LAB_ROOT/apps/observatory"
need_cluster

step "observatory image (local registry)"
# tagged by source content, so a code change is a new image and a rollout:
# everything the image is built from (Dockerfile, lockfile and build config
# included), not the build's own outputs (apps/observatory/.dockerignore)
tag=$(src_hash "$APP")
export OBSERVATORY_IMAGE="localhost:$LAB_REGISTRY_PORT/lab/observatory:$tag"
docker image inspect "$OBSERVATORY_IMAGE" >/dev/null 2>&1 || docker build -q -t "$OBSERVATORY_IMAGE" "$APP" >/dev/null \
  || die "building $OBSERVATORY_IMAGE failed"
docker push -q "$OBSERVATORY_IMAGE" >/dev/null || die "pushing $OBSERVATORY_IMAGE failed"
ok "$OBSERVATORY_IMAGE"

step "Platform identity: Keycloak realm ops (ops-identity)"
secret_apply ops-identity kc-secrets \
  KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret OPS_KC_ADMIN_PASSWORD)" \
  OBSERVATORY_CLIENT_SECRET="$(lab_secret OBSERVATORY_CLIENT_SECRET)" \
  GRAFANA_CLIENT_SECRET="$(lab_secret OPS_GRAFANA_CLIENT_SECRET)" \
  KIALI_CLIENT_SECRET="$(lab_secret OPS_KIALI_CLIENT_SECRET)" \
  SOLO_UI_CLIENT_SECRET="$(lab_secret OPS_SOLO_UI_CLIENT_SECRET)"
deploy_keycloak ops-identity "$OPS_DOMAIN" https-ops "$D/realm-ops.json"
secret_apply observatory observatory-oidc client-secret="$(lab_secret OBSERVATORY_CLIENT_SECRET)"
# its S&V service account, for Agent Substrate's status from kagent
secret_apply observatory kagent-client client-secret="$(lab_secret SV_OBSERVATORY_CLIENT_SECRET)"
ok "issuer https://idp.$OPS_DOMAIN/realms/ops  (admin: ops / ops-demo)"

step "Platform UIs on the edge, behind the admins' sign-in"
secret_apply observability grafana-sso client-secret="$(lab_secret OPS_GRAFANA_CLIENT_SECRET)"
secret_apply kiali kiali-sso client-secret="$(lab_secret OPS_KIALI_CLIENT_SECRET)"
apply_tmpl "$D/platform-uis.yaml"
ok "https://grafana.$OPS_DOMAIN  https://kiali.$OPS_DOMAIN  (ops / ops-demo)"

if [ -n "${ISTIO_UI_CHART:-}" ]; then
  # Solo Enterprise for Istio's own UI (ISTIO_EDITION=enterprise): it signs
  # the platform admins in itself, against the ops realm
  step "Solo Enterprise for Istio UI (solo-enterprise)"
  [ -n "$SOLO_ISTIO_LICENSE_KEY" ] || warn "no SOLO_ISTIO_LICENSE_KEY (or SOLO_LICENSE_KEY) in .env: the UI needs an Enterprise licence for Solo Enterprise for Istio"
  apply_tmpl "$D/solo-ui.yaml"
  deny_internet solo-enterprise
  printf '%s' "$SOLO_ISTIO_LICENSE_KEY" | K create secret generic solo-ui-license -n solo-enterprise \
    --from-file=license-key=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
  secret_apply solo-enterprise solo-ui-oidc clientSecret="$(lab_secret OPS_SOLO_UI_CLIENT_SECRET)"
  # the cluster as Istio names it, so the UI's graph matches the mesh's
  ISTIO_CLUSTER=$(K get deploy istiod -n istio-system -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLUSTER_ID")].value}')
  export ISTIO_CLUSTER
  SOLO_UI_AGENTGATEWAY=false; [ "$AGW_EDITION" = enterprise ] && SOLO_UI_AGENTGATEWAY=true
  export SOLO_UI_AGENTGATEWAY
  values_for "$D" values-solo-ui "$ISTIO_EDITION"
  helm_up solo-management "$ISTIO_UI_CHART" "$ISTIO_UI_VERSION" solo-enterprise "${VALS[@]}"
  ok "https://mesh.$OPS_DOMAIN  (ops / ops-demo)"
fi

step "Observatory"
deny_internet observatory ops-identity
# a binding's role can't change in place: one from before the scoped role goes
if [ "$(K get clusterrolebinding observatory-admins -o jsonpath='{.roleRef.name}' 2>/dev/null)" = cluster-admin ]; then
  K delete clusterrolebinding observatory-admins >/dev/null
fi
K apply -f "$D/rbac.yaml" >/dev/null
apply_tmpl "$D/admission.yaml"
apply_tmpl "$D/observatory.yaml"
apply_tmpl "$D/telemetry.yaml"
rollout observatory deploy/observatory
wait_for "https://observatory.$OPS_DOMAIN" 30 3 \
  sh -c "curl -s --cacert '$LAB_CA_DIR/ca.crt' -o /dev/null -w '%{http_code}' https://observatory.$OPS_DOMAIN/ | grep -q 302"
ok "https://observatory.$OPS_DOMAIN"
