#!/usr/bin/env bash
# Identity continuity checks: failover by a real network partition, failback,
# live rule changes, and Keycloak kept in step. The loop runs on a scratch tier
# pointed at an issuer the lab already runs (Ledgerline's IdP) and the spec is
# restored on exit. Checks what a browser would see, via the edge.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster
. "$LAB_ROOT/scripts/idp.sh"
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
lp=$(free_port); port_forward "$NS" keycloak "$lp" 80
KC="http://127.0.0.1:$lp"
kcadm() {
  local t; t=$(printf 'grant_type=client_credentials&client_id=continuity-controller&client_secret=%s' "$(lab_secret SV_CONTINUITY_CLIENT_SECRET)" \
    | curl -s "$KC/realms/sterling-vance/protocol/openid-connect/token" --data @- | jq -r .access_token)
  with_bearer "$t" curl -s "$KC/admin/realms/sterling-vance$1"
}
redirector() {
  local id; id=$(kcadm /authentication/flows/continuity-browser/executions | jq -r '.[] | select(.providerId=="identity-provider-redirector") | .authenticationConfig')
  kcadm "/authentication/config/$id" | jq -r '.config.defaultProvider // "none"'
}
idp_gone() { ! kcadm /identity-provider/instances | jq -e --arg t "$1" 'map(.alias) | index($t)' >/dev/null; }

# The upstream under test: the first IdP on the internet in the chain as
# installed, and the authorize endpoint its discovery publishes. LOCAL is the
# broker's break-glass tier (platform admins).
UP=$(idc | jq -r --arg tld ".$LAB_TLD" '[.spec.tiers[] | select(.type=="oidc" and ((.oidc.issuer | sub("^https://"; "") | split("/")[0] | endswith($tld)) | not))][0].name // empty')
LOCAL=$(idc | jq -r '[.spec.tiers[] | select(.type=="local")][0].name // empty')
UP_ISSUER=$(idc | jq -r --arg t "$UP" '.spec.tiers[] | select(.name==$t) | .oidc.issuer // empty')
UP_AUTHZ=$([ -n "$UP_ISSUER" ] && curl -s --max-time 10 "${UP_ISSUER%/}/.well-known/openid-configuration" | jq -r '.authorization_endpoint // empty')
has_up() { [ -n "$UP" ]; }
# partition <tier> <namespace> [serviceentry]: the kill switch. With a
# ServiceEntry it cuts only that upstream at S&V's egress. Without one (the
# in-lab scratch tier) it denies everything into <namespace> while the check
# runs, Ledgerline's own sign-ins included: there is no S&V-only path to cut.
partition() {
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

ORIG=$(idc | jq -c '{tiers: .spec.tiers, failback: .spec.failback, profile: .spec.profile, sync: .spec.sync}')
BASE=$(active)
cleanup() {
  heal "$T" "$TNS"; [ -n "$UP" ] && heal "$UP" sv-egress
  K patch idc "$IC" -n "$NS" --type merge -p "{\"spec\": $ORIG}" >/dev/null 2>&1
  K delete secret continuity-verify continuity-verify-directory -n "$NS" --ignore-not-found >/dev/null
  K delete job -n "$NS" -l continuity.lab.solo.io/verify=true --ignore-not-found >/dev/null
  secret_grant remove continuity-verify 2>/dev/null
  rm -f "$BODY" "$JAR"; kill "$(jobs -p)" 2>/dev/null
}
on_exit cleanup

step "The chain as installed"
expect '^2/2$' "controller: 2 replicas ready (one leader)" \
  "$(K get deploy continuity-controller -n "$NS" -o jsonpath='{.status.readyReplicas}/{.spec.replicas}')"
expect 'continuity-controller-' "leader holds Lease continuity.lab.solo.io" \
  "$(K get lease continuity.lab.solo.io -n "$NS" -o jsonpath='{.spec.holderIdentity}' 2>&1)"
expect '^True$' "IdentityContinuity $IC Ready" \
  "$(K get idc "$IC" -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
first=$(idc | jq -r '[.status.tiers[] | select(.configured and .healthy)][0].name // empty')
expect "^${first:-none}\$" "the first configured, healthy tier is active (${first:-none})" "$(active)"
expect "^$LOCAL\$" "the chain ends with the broker's break-glass accounts" "$(idc | jq -r '.spec.tiers[-1] | select(.type=="local") | .name')"
if [ "$(active)" = keycloak ]; then
  tok=$(sso_token bob bob-demo | jq -r .access_token)
  expect "^$BOB_ID keycloak\$" "Bob signs in through S&V's own IdP: his S&V user, session from keycloak" \
    "$(echo "$tok" | jq -rR 'split(".")[1] | gsub("-";"+") | gsub("_";"/") | @base64d | fromjson | "\(.sub) \(.idp)"' 2>/dev/null)"
  unset tok
else
  skipped "Bob's sign-in through $(active) is interactive (S&V's own IdP is not active)"
fi
if has_up; then
  expect '(Healthy|NotConfigured.*probe: Healthy)' "$UP upstream answers discovery + JWKS from S&V" "$(tier "$UP" reason): $(tier "$UP" message)"
else
  skipped "no IdP on the internet in the chain: set one's issuer and client (e.g. AUTH0_*) in .env and re-run make layer-47"
fi

# The loop runs on [scratch tier, break-glass]: the IdPs already in the
# chain are drained (out of rotation, their Keycloak IdPs and user links kept),
# so a partition of the scratch tier always falls back to break-glass.
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
expect '^[0-9]+s$' "probes see it: fails over to $LOCAL" "$(within 25 is_active "$LOCAL")"
expect '^false' "tier unhealthy" "$(tier "$T" healthy) $(tier "$T" reason)"
expect '^none$' "redirector cleared" "$(redirector)"
expect '^S&V login form$' "browser sign-in shows the broker's own form (break-glass)" "$(login_lands)"
expect "^$((fo+1))\$" "Event FailoverActivated" "$(events FailoverActivated)"
heal "$T" "$TNS"
expect '^[0-9]+s$' "healed: fails back to $T (Automatic)" "$(within 35 is_active "$T")"
expect "^$((fb+1))\$" "Event Failback" "$(events Failback)"

step "Rules take effect live"
K patch idc "$IC" -n "$NS" --type merge -p '{"spec":{"failback":"Manual"}}' >/dev/null
partition "$T" "$TNS"; within 25 is_active "$LOCAL" >/dev/null; heal "$T" "$TNS"
within 35 sh -c "[ \"\$(kubectl --context $KCTX get idc $IC -n $NS -o json | jq -r '.status.tiers[]|select(.name==\"$T\").healthy')\" = true ]" >/dev/null; sleep 6
expect "^$LOCAL\$" "failback Manual: healthy again, but stays on $LOCAL" "$(active)"
K patch idc "$IC" -n "$NS" --type merge -p '{"spec":{"failback":"Automatic"}}' >/dev/null
expect '^[0-9]+s$' "failback Automatic: moves back up" "$(within 10 is_active "$T")"
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"add","path":"/spec/tiers/0/drain","value":true}]' >/dev/null
expect '^[0-9]+s$' "drain: out of rotation" "$(within 10 is_active "$LOCAL")"
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"replace","path":"/spec/tiers/0/drain","value":false}]' >/dev/null
within 10 is_active "$T" >/dev/null
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"add","path":"/spec/tiers/0/failoverWhen/latencyAboveMs","value":1}]' >/dev/null
expect '^[0-9]+s SlowResponse$' "failoverWhen latencyAboveMs=1: slow counts, fails over" "$(within 25 is_active "$LOCAL") $(tier "$T" reason)"
K patch idc "$IC" -n "$NS" --type json -p '[{"op":"remove","path":"/spec/tiers/0/failoverWhen/latencyAboveMs"}]' >/dev/null
expect '^[0-9]+s$' "rule removed: back to $T" "$(within 35 is_active "$T")"

step "Profile, claim mapping and scheduled sync"
A=verifyDepartment SCOPE="$IC-profile" CJ="$IC-profile-sync"
K create secret generic continuity-verify-directory -n "$NS" --from-literal=client-id=verify --from-literal=client-secret="$(openssl rand -hex 16)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
K patch idc "$IC" -n "$NS" --type merge -p "$(jq -nc --arg a "$A" --arg t "$T" --argjson tiers "$(idc | jq -c .spec.tiers)" --arg kc "https://idp.$LEDGERLINE_DOMAIN/admin/realms/ledgerline" \
  '{spec: {profile: {attributes: [{name: $a, displayName: "Department (verify)"}], tokenClients: ["kagent"]},
    sync: {schedule: "0 3 * * *", suspend: true},
    tiers: ($tiers | map(if .name == $t then .claims = [{claim: "department", attribute: $a}, {claim: "given_name", attribute: "firstName"}]
      | .directory = {type: "keycloak", url: $kc, credentialsRef: {name: "continuity-verify-directory"}} else . end))}}')" >/dev/null
profile_has() { kcadm /users/profile | jq -e --arg a "$A" '.attributes[] | select(.name==$a) | .permissions.edit == ["admin"]'; }
expect '^[0-9]+s$' "user profile: $A added, users view only" "$(within 20 profile_has)"
mapper_mode() { kcadm "/identity-provider/instances/$T/mappers" | jq -r --arg n "claim-to-$A" '.[] | select(.name==$n) | .config.syncMode'; }
mapper_set() { [ "$(mapper_mode)" = FORCE ]; }
within 15 mapper_set >/dev/null
expect '^FORCE$' "IdP mapper claim-to-$A on $T: the first upstream updates it at every sign-in" "$(mapper_mode)"
kagent_scopes() { kcadm "/clients/$(kcadm '/clients?clientId=kagent' | jq -r '.[0].id')/default-client-scopes" | jq -r '.[].name'; }
expect "^$SCOPE\$" "client scope $SCOPE: a default scope of kagent (profile in its tokens)" "$(kagent_scopes | grep -x "$SCOPE")"
expect '^True$' "ProfileApplied" "$(K get idc "$IC" -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="ProfileApplied")].status}')"
cj_ready() { K get cronjob "$CJ" -n "$NS" >/dev/null; }
expect '^[0-9]+s$' "CronJob $CJ created" "$(within 15 cj_ready)"
expect '^0 3 \* \* \* Etc/UTC true Forbid IdentityContinuity continuity-sync$' "schedule UTC, suspended, no overlap, owned by the instance, runs as continuity-sync" \
  "$(K get cronjob "$CJ" -n "$NS" -o jsonpath='{.spec.schedule} {.spec.timeZone} {.spec.suspend} {.spec.concurrencyPolicy} {.metadata.ownerReferences[0].kind} {.spec.jobTemplate.spec.template.spec.serviceAccountName}')"
can() { K auth can-i get "secret/$2" -n "$NS" --as="system:serviceaccount:$NS:$1" 2>/dev/null; }
expect '^no no yes$' "sync reads only its Secrets: not the controller's or a tier's client secret" \
  "$(can continuity-sync continuity-controller) $(can continuity-sync continuity-verify) $(can continuity-sync continuity-sync)"
secret_grant_sync() {
  local r; r=$(K get role continuity-sync-secrets -n "$NS" -o json)
  echo "$r" | jq --arg n "$2" --arg op "$1" '.rules |= map(if (.resources | index("secrets")) then
      .resourceNames = (if $op == "add" then ((.resourceNames // []) + [$n] | unique) else ((.resourceNames // []) - [$n]) end) else . end)' \
    | K replace -f - >/dev/null
}
secret_grant_sync add continuity-verify-directory
expect '^no$' "the controller can't read a directory's credentials" "$(can continuity-controller continuity-verify-directory)"
J="$CJ-verify-$(date +%s)"
K create job "$J" -n "$NS" --from="cronjob/$CJ" >/dev/null && K label job "$J" -n "$NS" continuity.lab.solo.io/verify=true >/dev/null
job_done() { [ "$(K get job "$J" -n "$NS" -o jsonpath='{.status.succeeded}')" = 1 ]; }
expect '^[0-9]+s$' "a run completes (as continuity-sync, through the mesh to Keycloak)" "$(within 120 job_done)"
expect "^[0-9]+ [0-9]+ 0 .*users read" "status.sync: last run recorded, no failures" \
  "$(idc | jq -r '.status.sync | "\(.users // 0) \(.updated // 0) \(.failed // 0) \(.lastSuccess // "never") \(.message)"')"
# Test connection: a Job of the sync with --test-tier, on the sync's own path;
# the directory's token endpoint (Ledgerline's) refuses this made-up client
JT="$CJ-test-$(date +%s)"
K get cronjob "$CJ" -n "$NS" -o json | jq --arg n "$JT" --arg t "$T" '{apiVersion: "batch/v1", kind: "Job",
  metadata: {name: $n, labels: (.spec.jobTemplate.metadata.labels + {"continuity.lab.solo.io/verify": "true"})},
  spec: (.spec.jobTemplate.spec | .template.spec.containers[0].args += ["--test-tier=\($t)"])}' | K apply -n "$NS" -f - >/dev/null
test_done() { [ -n "$(K get job "$JT" -n "$NS" -o jsonpath='{.status.failed}{.status.succeeded}')" ]; }
within 120 test_done >/dev/null
expect '"ok":false.*directory token: HTTP 401' "directory test: run as the sync, reports the token endpoint's refusal (no user data)" \
  "$(K get pods -n "$NS" -l job-name="$JT" -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.message}')"
secret_grant_sync remove continuity-verify-directory

step "Removing the tier and profile cleans up"
K patch idc "$IC" -n "$NS" --type merge -p "{\"spec\": $ORIG}" >/dev/null
expect "^[0-9]+s\$" "active returns to $BASE" "$(within 15 is_active "$BASE")"
expect '^[0-9]+s$' "Keycloak IdP $T deleted" "$(within 15 idp_gone "$T")"
if [ "$(echo "$ORIG" | jq -r '.sync')" = null ]; then
  if echo "$ORIG" | jq -e '.tiers | any(.directory)' >/dev/null; then
    cj_suspended() { [ "$(K get cronjob "$CJ" -n "$NS" -o jsonpath='{.spec.suspend}')" = true ]; }
    expect '^[0-9]+s$' "CronJob $CJ kept for the directories as installed, never on a schedule" "$(within 15 cj_suspended)"
  else
    expect '^[0-9]+s$' "CronJob $CJ deleted" "$(within 15 sh -c "! kubectl --context $KCTX get cronjob $CJ -n $NS")"
  fi
fi
if [ "$(echo "$ORIG" | jq -r '.profile')" = null ]; then
  profile_gone() { ! profile_has && ! kagent_scopes | grep -qx "$SCOPE"; }
  expect '^[0-9]+s$' "profile attribute $A and scope $SCOPE removed" "$(within 20 profile_gone)"
fi

step "External upstream: ${UP:-none} through S&V's egress waypoint"
# a NotConfigured tier (no credentials) is still probed: its outcome is in the
# message ("...; probe: <result>")
if ! has_up; then
  skipped "no upstream tier: external upstream checks skipped"
elif K get gateway egress-waypoint -n sv-egress >/dev/null 2>&1; then
  expect '^egress-waypoint$' "ServiceEntry continuity-$UP bound to the egress waypoint" \
    "$(K get serviceentry "continuity-$UP" -n sv-egress -o jsonpath='{.metadata.labels.istio\.io/use-waypoint}' 2>&1)"
  partition "$UP" sv-egress "continuity-$UP"
  expect '^[0-9]+s$' "partition on the ServiceEntry: $UP probes fail" \
    "$(within 25 sh -c "kubectl --context $KCTX get idc $IC -n $NS -o json | jq -e '.status.tiers[]|select(.name==\"$UP\")|(.partitioned and ((.reason|test(\"Unreachable|ServerError\")) or (.message|test(\"probe: (Unreachable|ServerError)\"))))'")"
  heal "$UP" sv-egress
  expect '^[0-9]+s$' "healed: $UP probes pass again" \
    "$(within 35 sh -c "kubectl --context $KCTX get idc $IC -n $NS -o json | jq -e '.status.tiers[]|select(.name==\"$UP\")|(.message|test(\"discovery and jwks ok|probe: Healthy\"))'")"
else
  skipped "no sv-egress/egress-waypoint: external upstream checks skipped"
fi
if has_up && [ "$(tier "$UP" configured)" = true ] && [ "$(active)" = "$UP" ]; then
  expect "^${UP_AUTHZ}\\?.*broker%2F$UP%2Fendpoint" "browser sign-in goes to $UP, back to /broker/$UP/endpoint" "$(login_lands)"
  skipped "Bob's $UP sign-in itself is interactive: https://kagent.$SV_DOMAIN (lands on user $BOB_ID)"
else
  skipped "brokered sign-in via the upstream: set its client credentials in .env and re-run make layer-47"
fi

echo; [ $fail -eq 0 ] && ok "continuity: $pass/$((pass+fail)) checks passed${skip:+, $skip skipped}" || die "continuity: $fail of $((pass+fail)) checks failed"
