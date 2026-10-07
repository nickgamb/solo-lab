#!/usr/bin/env bash
# Identity continuity: S&V's broker (Keycloak) routes workforce sign-ins to an
# ordered chain of IdPs (ENTERPRISE_IDP, config/lab.env) and maps them into
# one profile; with every IdP down, only platform admins sign in, with the
# broker's break-glass accounts. The continuity controller (apps/continuity)
# health-checks each IdP and re-points the broker's login at the first healthy
# tier. The broker remains the issuer everything trusts. Needs 45-identity.
# Idempotent.
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
# the scheduled profile sync's realm client (users' profile attributes only)
K create secret generic continuity-sync -n sv-identity \
  --from-literal=client-id=continuity-sync \
  --from-literal=client-secret="$(lab_secret SV_CONTINUITY_SYNC_CLIENT_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# Auth0 as a directory (the directory sync): a Machine to Machine app for its
# Management API, in .env
if [ -n "${AUTH0_DIRECTORY_CLIENT_ID:-}" ] && [ -n "${AUTH0_DIRECTORY_CLIENT_SECRET:-}" ]; then
  K create secret generic directory-auth0 -n sv-identity \
    --from-literal=client-id="$AUTH0_DIRECTORY_CLIENT_ID" --from-literal=client-secret="$AUTH0_DIRECTORY_CLIENT_SECRET" \
    --dry-run=client -o yaml | K apply -f - >/dev/null
fi
# the sync reads S&V's workforce IdP's users with its view-users client there
K create secret generic directory-keycloak -n sv-identity \
  --from-literal=client-id=continuity-directory \
  --from-literal=client-secret="$(lab_secret SV_WORKFORCE_DIRECTORY_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
. "$LAB_ROOT/scripts/idp.sh"
CHAIN=$(idp_chain)
for n in $IDP_UPSTREAMS; do
  case " $CHAIN " in *" $n "*) ;; *) continue ;; esac
  N=$(echo "$n" | tr '[:lower:]' '[:upper:]')
  if [ -z "$(_idp_var "$n" CLIENT_ID)" ]; then
    warn "$n tier is NotConfigured until ${N}_CLIENT_ID is in .env (then re-run this layer)"
  elif [ "$(idp_client_auth "$n")" = private_key_jwt ]; then
    K delete secret "upstream-$n" -n sv-identity --ignore-not-found >/dev/null
    ok "$n: private_key_jwt with S&V's broker key (make xaa-keys for its JWKS)"
  else
    K create secret generic "upstream-$n" -n sv-identity \
      --from-literal=client-id="$(_idp_var "$n" CLIENT_ID)" --from-literal=client-secret="$(_idp_var "$n" CLIENT_SECRET)" \
      --dry-run=client -o yaml | K apply -f - >/dev/null
    ok "$n: client_secret_post (sv-identity/upstream-$n)"
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
# The tiers follow ENTERPRISE_IDP: which IdPs and in what order, then the
# broker's break-glass accounts (platform admins only). Each existing tier
# keeps its failover rules, claim mappings and directory (the operators' and
# the Observatory's); issuers and token settings follow .env. For an IdP that
# issues ID-JAGs the broker keeps the user's tokens (storeTokens) so the
# egress can have it vouch for them (docs/IDENTITY-FLOWS.md). No offline
# access: the stored refresh token lives and dies with the user's session at
# that IdP, so a logout there stops its ID-JAGs. S&V's own workforce IdP
# comes with its directory and an attribute mapping, for the directory sync.
desired=$({ for n in $CHAIN; do
  store=false; if echo " $IDP_ISSUES_IDJAG " | grep -q " $n "; then store=true; fi
  dir=null
  if [ "$n" = auth0 ] && [ -n "${AUTH0_DIRECTORY_CLIENT_ID:-}" ]; then
    a=${AUTH0_ISSUER%/}
    dir=$(jq -nc --arg a "$a" '{directory: {type: "auth0", url: "\($a)/api/v2", audience: "\($a)/api/v2/", credentialsRef: {name: "directory-auth0"}}}')
  fi
  if [ "$n" = keycloak ] && [ "$KEYCLOAK_ISSUER" = "https://login.$SV_DOMAIN/realms/workforce" ]; then
    dir='{"directory": {"type": "keycloak", "url": "http://keycloak.sv-workforce.svc/admin/realms/workforce", "credentialsRef": {"name": "directory-keycloak"}},
      "attributes": [{"attribute": "email", "path": "email"}, {"attribute": "firstName", "path": "firstName"}, {"attribute": "lastName", "path": "lastName"}]}'
  fi
  jq -nc --arg n "$n" --arg iss "$(_idp_var "$n" ISSUER)" --arg cid "$(_idp_var "$n" CLIENT_ID)" \
    --arg auth "$(idp_client_auth "$n")" --argjson store "$store" --argjson dir "$dir" '{name: $n,
    displayName: ({okta: "Okta", auth0: "Auth0", gluu: "Gluu", keycloak: "Sterling & Vance (Keycloak)"}[$n] // $n), type: "oidc",
    oidc: ({issuer: $iss, clientID: $cid, clientAuth: $auth}
      + (if $auth == "private_key_jwt" then {clientAssertionSigningAlg: "PS256"} else {clientSecretRef: {name: "upstream-\($n)"}} end)
      + (if $store then {scopes: ["openid", "email", "profile"], storeTokens: true} else {} end)),
    failoverWhen: {unreachable: true, serverError: true, invalidDiscovery: true, latencyAboveMs: 1500}}
    + ($dir // {})'
done
jq -nc '{name: "break-glass", displayName: "Platform admins (break-glass)", type: "local"}'
} | jq -sc .)
if ! K get identitycontinuity sterling-vance -n sv-identity >/dev/null 2>&1; then
  render "$D/identitycontinuity.yaml" | T="$desired" yq '.spec.tiers = env(T)' | K apply -f - >/dev/null
else
  cur=$(K get idc sterling-vance -n sv-identity -o json | jq -c '.spec.tiers')
  # an existing tier keeps what operators set (display name, enabled, failover
  # rules); .env sets its type, issuer, secret and token settings
  merged=$(jq -nc --argjson d "$desired" --argjson c "$cur" '$d | map(. as $t
    | ([$c[] | select(.name == $t.name and .type == $t.type)][0]) as $old
    | if $old == null then $t
      else ($old * ($t | del(.displayName, .failoverWhen) | if $old.directory then del(.directory) else . end
          | if $old.attributes then del(.attributes) else . end))
        | if .oidc.clientAuth == "private_key_jwt" then del(.oidc.clientSecretRef) else . end end)')
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
