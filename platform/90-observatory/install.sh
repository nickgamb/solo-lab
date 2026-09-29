#!/usr/bin/env bash
# The Observatory: a live, single pane of glass over the whole lab, for
# platform admins. Its own Keycloak (realm ops, ops-identity) signs admins in
# at the edge; it reads the cluster as itself and changes it only as the
# signed-in admin. Every gateway exports its access log to it through the
# collector. Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
APP="$LAB_ROOT/apps/observatory"
need_cluster

step "observatory image (local registry)"
# tagged by source content, so a code change is a new image and a rollout
tag=$(cd "$APP" && find server web/src web/index.html web/package.json web/public -type f -not -path 'server/web/*' | LC_ALL=C sort | xargs cat | shasum | cut -c1-12)
export OBSERVATORY_IMAGE="localhost:$LAB_REGISTRY_PORT/lab/observatory:$tag"
docker image inspect "$OBSERVATORY_IMAGE" >/dev/null 2>&1 || docker build -q -t "$OBSERVATORY_IMAGE" "$APP" >/dev/null
docker push -q "$OBSERVATORY_IMAGE" >/dev/null
ok "$OBSERVATORY_IMAGE"

step "Platform identity: Keycloak realm ops (ops-identity)"
K create secret generic kc-secrets -n ops-identity \
  --from-literal=KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  --from-literal=KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret OPS_KC_ADMIN_PASSWORD)" \
  --from-literal=OBSERVATORY_CLIENT_SECRET="$(lab_secret OBSERVATORY_CLIENT_SECRET)" \
  --from-literal=GRAFANA_CLIENT_SECRET="$(lab_secret OPS_GRAFANA_CLIENT_SECRET)" \
  --from-literal=KIALI_CLIENT_SECRET="$(lab_secret OPS_KIALI_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
deploy_keycloak ops-identity "$OPS_DOMAIN" https-ops "$D/realm-ops.json"
K create secret generic observatory-oidc -n observatory \
  --from-literal=client-secret="$(lab_secret OBSERVATORY_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
ok "issuer https://idp.$OPS_DOMAIN/realms/ops  (admin: ops / ops-demo)"

step "Platform UIs on the edge, behind the admins' sign-in"
K create secret generic grafana-sso -n observability --from-literal=client-secret="$(lab_secret OPS_GRAFANA_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
K create secret generic kiali-sso -n istio-system --from-literal=client-secret="$(lab_secret OPS_KIALI_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
apply_tmpl "$D/platform-uis.yaml"
ok "https://grafana.$OPS_DOMAIN  https://kiali.$OPS_DOMAIN  (ops / ops-demo)"

step "Observatory"
deny_internet observatory ops-identity
K apply -f "$D/rbac.yaml" >/dev/null
apply_tmpl "$D/observatory.yaml"
K apply -f "$D/telemetry.yaml" >/dev/null
rollout observatory deploy/observatory
wait_for "https://observatory.$OPS_DOMAIN" 30 3 \
  sh -c "curl -s -o /dev/null -w '%{http_code}' https://observatory.$OPS_DOMAIN/ | grep -q 302"
ok "https://observatory.$OPS_DOMAIN"
