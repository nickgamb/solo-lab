#!/usr/bin/env bash
# Identity continuity: S&V's Keycloak brokers workforce logins to an ordered
# chain of upstream IdPs (ENTERPRISE_IDP, config/lab.env) and falls back to
# its own accounts. The
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
. "$LAB_ROOT/scripts/idp.sh"
CHAIN=$(idp_chain)
for n in $IDP_UPSTREAMS; do
  case " $CHAIN " in *" $n "*) ;; *) continue ;; esac
  N=$(echo "$n" | tr '[:lower:]' '[:upper:]')
  if [ -n "$(_idp_var "$n" CLIENT_ID)" ] && [ -n "$(_idp_var "$n" CLIENT_SECRET)" ]; then
    K create secret generic "upstream-$n" -n sv-identity \
      --from-literal=client-id="$(_idp_var "$n" CLIENT_ID)" --from-literal=client-secret="$(_idp_var "$n" CLIENT_SECRET)" \
      --dry-run=client -o yaml | K apply -f - >/dev/null
    ok "sv-identity/upstream-$n"
  else
    warn "$n tier is NotConfigured until ${N}_CLIENT_ID and ${N}_CLIENT_SECRET are in .env (then re-run this layer)"
  fi
done
ok "sv-identity/continuity-controller  (chain: $(echo "$CHAIN" | sed 's/ / -> /g'))"

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
# The tiers follow ENTERPRISE_IDP: which IdPs and in what order. Each existing
# tier keeps its failover rules (the operators' and the Observatory's rule
# builder's); issuers and token settings follow .env. An upstream that issues
# ID-JAGs keeps the user's tokens (storeTokens, with a refresh token) so the
# egress can have it vouch for them (docs/IDENTITY-FLOWS.md).
desired=$(for n in $CHAIN; do
  if [ "$n" = keycloak ]; then jq -nc '{name: "keycloak", displayName: "Sterling & Vance (local)", type: "local"}'
  else
    store=false; if echo " $IDP_ISSUES_IDJAG " | grep -q " $n "; then store=true; fi
    jq -nc --arg n "$n" --arg iss "$(_idp_var "$n" ISSUER)" --argjson store "$store" '{name: $n,
      displayName: ({auth0: "Auth0", gluu: "Gluu"}[$n] // $n), type: "oidc",
      oidc: ({issuer: $iss, clientSecretRef: {name: "upstream-\($n)"}}
        + (if $store then {scopes: ["openid", "email", "profile", "offline_access"], storeTokens: true} else {} end)),
      failoverWhen: {unreachable: true, serverError: true, invalidDiscovery: true, latencyAboveMs: 1500}}'
  fi
done | jq -sc .)
if ! K get identitycontinuity sterling-vance -n sv-identity >/dev/null 2>&1; then
  render "$D/identitycontinuity.yaml" | T="$desired" yq '.spec.tiers = env(T)' | K apply -f - >/dev/null
else
  cur=$(K get idc sterling-vance -n sv-identity -o json | jq -c '.spec.tiers')
  # an existing tier keeps what operators set (display name, enabled, failover
  # rules); .env sets its type, issuer, secret and token settings
  merged=$(jq -nc --argjson d "$desired" --argjson c "$cur" '$d | map(. as $t
    | ([$c[] | select(.name == $t.name)][0]) as $old
    | if $old == null then $t else $old * ($t | del(.displayName, .failoverWhen)) end)')
  # every S&V caller of an upstream goes through the egress waypoint
  egress=$(render "$D/identitycontinuity.yaml" | yq -o json -I0 '.spec.egress')
  if [ "$(K get idc sterling-vance -n sv-identity -o json | jq -cS '.spec.egress | del(.internalDomains)')" != "$(echo "$egress" | jq -cS .)" ]; then
    K patch idc sterling-vance -n sv-identity --type merge -p "{\"spec\":{\"egress\":$egress}}" >/dev/null
    ok "egress: $(echo "$egress" | jq -c .)"
  fi
  if [ "$(echo "$cur" | jq -cS .)" != "$(echo "$merged" | jq -cS .)" ]; then
    K patch idc sterling-vance -n sv-identity --type merge -p "{\"spec\":{\"tiers\":$merged}}" >/dev/null
    ok "tiers: $(echo "$cur" | jq -r 'map(.name) | join(" -> ")') => $(echo "$merged" | jq -r 'map(.name) | join(" -> ")')"
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
