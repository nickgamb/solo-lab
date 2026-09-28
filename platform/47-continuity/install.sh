#!/usr/bin/env bash
# Identity continuity: S&V's Keycloak brokers workforce logins to an ordered
# chain of upstream IdPs (Auth0 first) and falls back to its own accounts. The
# continuity controller (apps/continuity) health-checks each upstream and
# re-points Keycloak's login at the first healthy tier. Keycloak remains the
# issuer everything trusts. Needs 45-identity. Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
APP="$LAB_ROOT/apps/continuity"
need_cluster

step "continuity-controller image (local registry)"
# tagged by source content, so a code change is a new image and a rollout
tag=$(cd "$APP" && find . -type f | LC_ALL=C sort | xargs cat | shasum | cut -c1-12)
export CONTINUITY_IMAGE="localhost:$LAB_REGISTRY_PORT/lab/continuity-controller:$tag"
docker image inspect "$CONTINUITY_IMAGE" >/dev/null 2>&1 || docker build -q -t "$CONTINUITY_IMAGE" "$APP" >/dev/null
docker push -q "$CONTINUITY_IMAGE" >/dev/null
ok "$CONTINUITY_IMAGE (unit tests ran in the build)"

step "Credentials"
# the controller's own realm client (defined in realm-sterling-vance.json)
K create secret generic continuity-controller -n sv-identity \
  --from-literal=client-id=continuity-controller \
  --from-literal=client-secret="$(lab_secret SV_CONTINUITY_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
if [ -n "${AUTH0_CLIENT_ID:-}" ] && [ -n "${AUTH0_CLIENT_SECRET:-}" ]; then
  K create secret generic upstream-auth0 -n sv-identity \
    --from-literal=client-id="$AUTH0_CLIENT_ID" --from-literal=client-secret="$AUTH0_CLIENT_SECRET" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
  ok "sv-identity/continuity-controller  sv-identity/upstream-auth0"
else
  ok "sv-identity/continuity-controller"
  warn "auth0 tier is NotConfigured until AUTH0_CLIENT_ID and AUTH0_CLIENT_SECRET are in .env (then re-run this layer)"
fi

step "Egress waypoint (sv-egress)"
apply_tmpl "$D/egress.yaml"
wait_for "egress waypoint Programmed" 40 3 K wait -n sv-egress gateway/egress-waypoint --for=condition=Programmed --timeout=2s
ok "external upstreams leave through sv-egress/egress-waypoint"

step "IdentityContinuity CRD + controller (sv-identity)"
K apply --server-side -f "$APP/config/crd" >/dev/null
K wait crd/identitycontinuities.continuity.lab.solo.io --for=condition=Established --timeout=60s >/dev/null
apply_tmpl "$D/controller.yaml"
rollout sv-identity deploy/continuity-controller
K get identitycontinuity sterling-vance -n sv-identity >/dev/null 2>&1 || apply_tmpl "$D/identitycontinuity.yaml"
wait_for "sterling-vance continuity Ready" 30 2 \
  sh -c "kubectl --context $KCTX get idc sterling-vance -n sv-identity -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' | grep -q True"
ok "active tier: $(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}')  (kubectl get idc -n sv-identity)"
