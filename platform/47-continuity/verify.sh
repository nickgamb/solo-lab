#!/usr/bin/env bash
# Identity continuity checks: failover by a real network partition, failback,
# live rule changes, and Keycloak kept in step. The loop runs on a scratch tier
# pointed at an issuer the lab already runs (Ledgerline's IdP) and the spec is
# restored on exit. Checks what a browser would see, via the edge.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster; need_password_grant
NS=sv-identity IC=sterling-vance BOB_ID=5b0b0000-0000-4000-8000-000000000b0b
T=verify-ledgerline TNS=ledgerline-identity
CA=(--cacert "$LAB_CA_DIR/ca.crt")
BODY=$(mktemp) JAR=$(mktemp)
pass=0 fail=0 skip=0
res() { if [ "$1" = ok ]; then ok "$2"; pass=$((pass+1)); else warn "$2"; echo "      got: ${3:0:300}"; fail=$((fail+1)); fi; }
expect() { echo "$3" | grep -qE "$1" && res ok "$2" || res no "$2" "$3"; }
skipped() { printf '  - %s\n' "$*"; skip=$((skip+1)); }

idc() { K get idc "$IC" -n "$NS" -o json; }
active() { K get idc "$IC" -n "$NS" -o jsonpath='{.status.active}'; }
tier() { idc | jq -r --arg t "$1" --arg f "$2" '.status.tiers[] | select(.name==$t) | .[$f]'; }
within() {  # within <seconds> <cmd...>: "<n>s" once cmd succeeds, else "timeout"
  local s; s=$(date +%s)
  while [ $(( $(date +%s) - s )) -lt "$1" ]; do "${@:2}" >/dev/null 2>&1 && { echo "$(( $(date +%s) - s ))s"; return; }; sleep 1; done
  echo "timeout"
}
is_active() { [ "$(active)" = "$1" ]; }
events() { K get events -n "$NS" --field-selector "involvedObject.name=$IC,reason=$1" -o name | wc -l | tr -d ' '; }
# Where a fresh browser sign-in to kagent lands: S&V's own form, or the
# first URL off idp.sterling.lab (an upstream IdP).
login_lands() {
  local url="https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/auth?client_id=kagent&response_type=code&scope=openid&redirect_uri=https%3A%2F%2Fkagent.$SV_DOMAIN%2Foauth2%2Fredirect&code_challenge=vErIfYvErIfYvErIfYvErIfYvErIfYvErIfYvErIf00&code_challenge_method=S256"
  local out loc i
  : > "$JAR"
  for i in 1 2 3 4; do
    out=$(curl -s "${CA[@]}" -b "$JAR" -c "$JAR" -D - -o "$BODY" "$url"); loc=$(echo "$out" | awk 'tolower($1)=="location:"{print $2}' | tr -d '\r')
    if [ -z "$loc" ]; then grep -q 'kc-form-login' "$BODY" && echo "S&V login form" || echo "$out" | head -1; return; fi
    case "$loc" in "https://idp.$SV_DOMAIN/"*) url=$loc ;; *) echo "$loc"; return ;; esac
  done
}
lands_on_form() { [ "$(login_lands)" = "S&V login form" ]; }

# Keycloak as the controller sees it (its own service-account client)
lp=$((18000 + RANDOM % 1000)); port_forward "$NS" keycloak "$lp" 80
KC="http://127.0.0.1:$lp"
kcadm() {
  local t; t=$(curl -s "$KC/realms/sterling-vance/protocol/openid-connect/token" -d grant_type=client_credentials \
    -d client_id=continuity-controller -d client_secret="$(lab_secret SV_CONTINUITY_CLIENT_SECRET)" | jq -r .access_token)
  curl -s -H "authorization: Bearer $t" "$KC/admin/realms/sterling-vance$1"
}
redirector() {
  local id; id=$(kcadm /authentication/flows/continuity-browser/executions | jq -r '.[] | select(.providerId=="identity-provider-redirector") | .authenticationConfig')
  kcadm "/authentication/config/$id" | jq -r '.config.defaultProvider // "none"'
}
idp_gone() { ! kcadm /identity-provider/instances | jq -e --arg t "$1" 'map(.alias) | index($t)' >/dev/null; }

partition() {  # partition <tier> <namespace> [serviceentry]: the kill switch
  local target=""; [ -n "${3:-}" ] && target="targetRefs: [{group: networking.istio.io, kind: ServiceEntry, name: $3}]"
  K apply -f - >/dev/null <<YAML
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata: {name: continuity-partition-$1, namespace: $2, labels: {continuity.lab.solo.io/tier: $1}}
spec: {action: DENY, rules: [{}], $target}
YAML
}
heal() { K delete authorizationpolicy "continuity-partition-$1" -n "$2" --ignore-not-found >/dev/null; }
# The controller reads Secrets by name only (Role continuity-controller-secrets):
# a new tier's Secret is granted, the way the Observatory's rule builder does it.
secret_grant() {  # secret_grant add|remove <name>
  local r; r=$(K get role continuity-controller-secrets -n "$NS" -o json)
  echo "$r" | jq --arg n "$2" --arg op "$1" '.rules |= map(if (.resources | index("secrets")) then
      .resourceNames = (if $op == "add" then ((.resourceNames // []) + [$n] | unique) else ((.resourceNames // []) - [$n]) end) else . end)' \
    | K replace -f - >/dev/null
}

ORIG=$(idc | jq -c '{tiers: .spec.tiers, failback: .spec.failback}')
BASE=$(active)
cleanup() {
  heal "$T" "$TNS"; heal auth0 sv-egress
  K patch idc "$IC" -n "$NS" --type merge -p "{\"spec\": $ORIG}" >/dev/null 2>&1
  K delete secret continuity-verify -n "$NS" --ignore-not-found >/dev/null
  secret_grant remove continuity-verify 2>/dev/null
  rm -f "$BODY" "$JAR"; kill "$(jobs -p)" 2>/dev/null
}
trap cleanup EXIT

step "The chain as installed"
expect '^2/2$' "controller: 2 replicas ready (one leader)" \
  "$(K get deploy continuity-controller -n "$NS" -o jsonpath='{.status.readyReplicas}/{.spec.replicas}')"
expect 'continuity-controller-' "leader holds Lease continuity.lab.solo.io" \
  "$(K get lease continuity.lab.solo.io -n "$NS" -o jsonpath='{.spec.holderIdentity}' 2>&1)"
expect '^True$' "IdentityContinuity $IC Ready" \
  "$(K get idc "$IC" -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
if [ "$(tier auth0 configured)" = true ]; then
  expect '^auth0 \(Healthy\)$' "auth0 configured and healthy: it is active" "$(active) ($(tier auth0 reason))"
else
  expect '^keycloak ' "auth0 NotConfigured (no upstream-auth0 secret): active is local" "$(active) (auth0: $(tier auth0 reason))"
fi
expect '(Healthy|NotConfigured.*probe: Healthy)' "auth0 upstream answers discovery + JWKS from S&V" "$(tier auth0 reason): $(tier auth0 message)"
tok=$(curl -s "$KC/realms/sterling-vance/protocol/openid-connect/token" -d grant_type=password -d client_id=kagent \
  -d client_secret="$(lab_secret SV_KAGENT_CLIENT_SECRET)" -d username=bob -d password=bob-demo -d scope=openid | jq -r .access_token)
expect "^$BOB_ID\$" "password grant unchanged: Bob's S&V user id" "$(echo "$tok" | jq -rR 'split(".")[1] | gsub("-";"+") | gsub("_";"/") | @base64d | fromjson | .sub' 2>/dev/null)"
unset tok

# The loop runs on [scratch tier, local accounts]: upstream tiers already in the
# chain are drained (out of rotation, their Keycloak IdPs and user links kept),
# so a partition of the scratch tier always falls back to S&V's own form.
step "A new tier at runtime (spec edit + its Secret, no restart)"
K create secret generic continuity-verify -n "$NS" --from-literal=client-secret="$(openssl rand -hex 16)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
secret_grant add continuity-verify
K patch idc "$IC" -n "$NS" --type merge -p "$(echo "$ORIG" | jq -c --arg t "$T" --arg iss "https://idp.$LEDGERLINE_DOMAIN/realms/ledgerline" \
  '{spec: {failback: "Automatic", tiers: ([{name: $t, displayName: "verify (Ledgerline IdP)", type: "oidc",
    oidc: {issuer: $iss, clientID: "continuity-verify", clientSecretRef: {name: "continuity-verify"}}}]
    + (.tiers | map(if .type == "oidc" then .drain = true else . end)))}}')" >/dev/null
expect '^[0-9]+s$' "$T becomes active" "$(within 20 is_active "$T")"
expect '^verify-ledgerline$' "Keycloak redirector -> $T" "$(redirector)"
expect "^https://idp\.$LEDGERLINE_DOMAIN/realms/ledgerline/.*client_id=continuity-verify.*broker%2F$T%2Fendpoint" \
  "browser sign-in goes straight to the upstream, back to /broker/$T/endpoint" "$(login_lands)"

step "Kill switch: partition the upstream (DENY policy in $TNS)"
fo=$(events FailoverActivated) fb=$(events Failback)
partition "$T" "$TNS"
expect '^[0-9]+s$' "probes see it: fails over to keycloak" "$(within 25 is_active keycloak)"
expect '^false true' "tier unhealthy, partitioned (informational)" "$(tier "$T" healthy) $(tier "$T" partitioned) $(tier "$T" reason)"
expect '^none$' "redirector cleared" "$(redirector)"
expect '^S&V login form$' "browser sign-in shows S&V's own form" "$(login_lands)"
expect "^$((fo+1))\$" "Event FailoverActivated" "$(events FailoverActivated)"
heal "$T" "$TNS"
expect '^[0-9]+s$' "healed: fails back to $T (Automatic)" "$(within 35 is_active "$T")"
expect "^$((fb+1))\$" "Event Failback" "$(events Failback)"

step "Rules take effect live"
K patch idc "$IC" -n "$NS" --type merge -p '{"spec":{"failback":"Manual"}}' >/dev/null
partition "$T" "$TNS"; within 25 is_active keycloak >/dev/null; heal "$T" "$TNS"
within 35 sh -c "[ \"\$(kubectl --context $KCTX get idc $IC -n $NS -o json | jq -r '.status.tiers[]|select(.name==\"$T\").healthy')\" = true ]" >/dev/null; sleep 6
expect '^keycloak$' "failback Manual: healthy again, but stays on keycloak" "$(active)"
K patch idc "$IC" -n "$NS" --type merge -p '{"spec":{"failback":"Automatic"}}' >/dev/null
expect '^[0-9]+s$' "failback Automatic: moves back up" "$(within 10 is_active "$T")"
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"add","path":"/spec/tiers/0/drain","value":true}]' >/dev/null
expect '^[0-9]+s$' "drain: out of rotation" "$(within 10 is_active keycloak)"
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"replace","path":"/spec/tiers/0/drain","value":false}]' >/dev/null
within 10 is_active "$T" >/dev/null
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"add","path":"/spec/tiers/0/failoverWhen/latencyAboveMs","value":1}]' >/dev/null
expect '^[0-9]+s SlowResponse$' "failoverWhen latencyAboveMs=1: slow counts, fails over" "$(within 25 is_active keycloak) $(tier "$T" reason)"
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"remove","path":"/spec/tiers/0/failoverWhen/latencyAboveMs"}]' >/dev/null
expect '^[0-9]+s$' "rule removed: back to $T" "$(within 35 is_active "$T")"

step "Removing the tier cleans up"
K patch idc "$IC" -n "$NS" --type merge -p "{\"spec\": $ORIG}" >/dev/null
expect "^[0-9]+s\$" "active returns to $BASE" "$(within 15 is_active "$BASE")"
expect '^[0-9]+s$' "Keycloak IdP $T deleted" "$(within 15 idp_gone "$T")"

step "External upstream: auth0 through S&V's egress waypoint"
if K get gateway egress-waypoint -n sv-egress >/dev/null 2>&1; then
  expect '^egress-waypoint$' "ServiceEntry continuity-auth0 bound to the egress waypoint" \
    "$(K get serviceentry continuity-auth0 -n sv-egress -o jsonpath='{.metadata.labels.istio\.io/use-waypoint}' 2>&1)"
  partition auth0 sv-egress continuity-auth0
  expect '^[0-9]+s$' "partition on the ServiceEntry: auth0 probes fail" \
    "$(within 25 sh -c "kubectl --context $KCTX get idc $IC -n $NS -o json | jq -e '.status.tiers[]|select(.name==\"auth0\")|(.partitioned and (.reason|test(\"Unreachable|ServerError\")))'")"
  heal auth0 sv-egress
  expect '^[0-9]+s$' "healed: auth0 probes pass again" \
    "$(within 35 sh -c "kubectl --context $KCTX get idc $IC -n $NS -o json | jq -e '.status.tiers[]|select(.name==\"auth0\")|(.message|test(\"discovery and jwks ok\"))'")"
else
  skipped "no sv-egress/egress-waypoint: external upstream checks skipped"
fi
if [ "$(tier auth0 configured)" = true ] && [ "$(active)" = auth0 ]; then
  expect "^${AUTH0_ISSUER}authorize\?.*broker%2Fauth0%2Fendpoint" "browser sign-in goes to Auth0, back to /broker/auth0/endpoint" "$(login_lands)"
  skipped "Bob's Auth0 sign-in itself is interactive: https://kagent.$SV_DOMAIN (lands on user $BOB_ID)"
else
  skipped "brokered sign-in via Auth0: set AUTH0_CLIENT_ID/AUTH0_CLIENT_SECRET in .env and re-run make layer-47"
fi

echo; [ $fail -eq 0 ] && ok "continuity: $pass/$((pass+fail)) checks passed${skip:+, $skip skipped}" || die "continuity: $fail of $((pass+fail)) checks failed"
