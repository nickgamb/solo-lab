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
tag=$(cd "$APP" && find . -type f -not -name .DS_Store | LC_ALL=C sort | xargs cat | sha1 | cut -c1-12)
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
# partitions are read in the egress namespace only; a lab from before that
# still has the cluster-wide grant
K delete clusterrolebinding,clusterrole continuity-controller-partitions --ignore-not-found >/dev/null
rollout sv-identity deploy/continuity-controller
if ! K get identitycontinuity sterling-vance -n sv-identity >/dev/null 2>&1; then
  # without an issuer there is no upstream tier, only S&V's own accounts
  if [ -n "$AUTH0_ISSUER" ]; then render "$D/identitycontinuity.yaml"
  else render "$D/identitycontinuity.yaml" | yq 'del(.spec.tiers[] | select(.name == "auth0"))'; fi | K apply -f - >/dev/null
elif [ -n "$AUTH0_ISSUER" ]; then
  # the spec is the operators' now; only follow a changed AUTH0_ISSUER
  i=$(K get idc sterling-vance -n sv-identity -o json | jq '[.spec.tiers[].name] | index("auth0") // empty')
  cur=$([ -n "$i" ] && K get idc sterling-vance -n sv-identity -o jsonpath="{.spec.tiers[$i].oidc.issuer}")
  if [ -z "$i" ]; then warn "the auth0 tier was removed from the IdentityContinuity; AUTH0_ISSUER is not applied"
  elif [ "$cur" != "$AUTH0_ISSUER" ]; then
    K patch idc sterling-vance -n sv-identity --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/tiers/$i/oidc/issuer\",\"value\":\"$AUTH0_ISSUER\"}]" >/dev/null
    ok "auth0 tier issuer: $cur -> $AUTH0_ISSUER"
  fi
fi
# a latency rule at or above the probe timeout can never fire (the probe times
# out first): earlier versions installed one, so bring it under the timeout
fix=$(K get idc sterling-vance -n sv-identity -o json | jq -c '((.spec.health.timeoutSeconds // 2) * 1000) as $t
  | [.spec.tiers | to_entries[] | select((.value.failoverWhen.latencyAboveMs // 0) >= $t)
     | {op: "replace", path: "/spec/tiers/\(.key)/failoverWhen/latencyAboveMs", value: ($t * 3 / 4 | floor)}]')
if [ "$fix" != "[]" ]; then K patch idc sterling-vance -n sv-identity --type json -p "$fix" >/dev/null; ok "latencyAboveMs brought under the probe timeout"; fi
wait_for "sterling-vance continuity Ready" 30 2 \
  sh -c "kubectl --context $KCTX get idc sterling-vance -n sv-identity -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' | grep -q True"
ok "active tier: $(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}')  (kubectl get idc -n sv-identity)"
