#!/usr/bin/env bash
# Identity continuity checks, on S&V's own IdPs: failover by a real network
# partition, failback, live rule changes, the directory sync, and the broker
# kept in step. The loop runs on S&V's own Keycloak (in the lab, so it can be
# cut); the spec is restored on exit. Checks what a browser would see, via the
# edge.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster
. "$LAB_ROOT/scripts/idp.sh"
NS=sv-identity IC=sterling-vance BOB_ID=5b0b0000-0000-4000-8000-000000000b0b
T=keycloak TNS=sv-workforce   # S&V's own IdP, cut by a DENY in its namespace
CA=(--cacert "$LAB_CA_DIR/ca.crt")
TMPD=$(umask 077; mktemp -d); on_exit "rm -rf $TMPD"   # every temp file, gone on exit
BODY=$TMPD/body JAR=$TMPD/jar
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
# events <reason> <since, epoch seconds>: how many were recorded since then
# (counted by time, not before/after totals: events expire after an hour)
events() {
  K get events -n "$NS" --field-selector "involvedObject.name=$IC,reason=$1" -o json \
    | jq --argjson s "$2" '[.items[] | (.eventTime // .firstTimestamp) | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 | select(. >= $s)] | length'
}
# Where a fresh browser sign-in to kagent lands: S&V's own form, or the
# first URL off idp.sterling.lab (an upstream IdP).
login_lands() {
  local url="https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/auth?client_id=kagent&response_type=code&scope=openid&redirect_uri=https%3A%2F%2Fkagent.$SV_DOMAIN%2Foauth2%2Fredirect&code_challenge=vErIfYvErIfYvErIfYvErIfYvErIfYvErIfYvErIf00&code_challenge_method=S256"
  local out loc
  : > "$JAR"
  for _ in 1 2 3 4; do
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
  local t; t=$(printf 'grant_type=client_credentials&client_id=continuity-controller&client_secret=%s' "$(lab_secret_get SV_CONTINUITY_CLIENT_SECRET)" \
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
# its discovery, from this host: an upstream on the internet that doesn't
# answer is a failed check below, not the end of the run
UP_AUTHZ=""
if [ -n "$UP_ISSUER" ]; then
  UP_AUTHZ=$(curl -sf --max-time 10 "${UP_ISSUER%/}/.well-known/openid-configuration" | jq -r '.authorization_endpoint // empty' 2>/dev/null) || UP_AUTHZ=""
fi
has_up() { [ -n "$UP" ]; }
# partition <tier> <namespace> [serviceentry]: the kill switch. With a
# ServiceEntry it cuts only that upstream at S&V's egress. Without one (an
# in-lab IdP, S&V's own Keycloak) it denies everything into its namespace
# while the check runs.
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
ORIG=$(idc | jq -c '{tiers: .spec.tiers, failback: .spec.failback, profile: .spec.profile, sync: .spec.sync}')
BASE=$(active)
KI=$(echo "$ORIG" | jq '[.tiers[].name] | index("keycloak") // empty')   # its place in the chain
[ -n "$KI" ] || die "S&V's own Keycloak (keycloak) isn't in ENTERPRISE_IDP: these checks run on it"
cleanup() {
  heal "$T" "$TNS"; [ -n "$UP" ] && heal "$UP" sv-egress
  K patch idc "$IC" -n "$NS" --type merge -p "{\"spec\": $ORIG}" >/dev/null 2>&1
  K delete job -n "$NS" -l continuity.lab.solo.io/verify=true --ignore-not-found >/dev/null
  # shellcheck disable=SC2046  # one pid per word
  kill $(jobs -p) 2>/dev/null
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
if has_up; then
  expect '^https://' "$UP discovery answers from this host (${UP_ISSUER%/})" "${UP_AUTHZ:-no answer from ${UP_ISSUER%/}/.well-known/openid-configuration}"
  expect '(Healthy|NotConfigured.*probe: Healthy)' "$UP upstream answers discovery + JWKS from S&V" "$(tier "$UP" reason): $(tier "$UP" message)"
else
  skipped "no IdP on the internet in the chain: set one's issuer and client (e.g. AUTH0_*) in .env and re-run make layer-47"
fi

# The loop runs on S&V's own Keycloak with the IdPs ahead of it drained (out
# of rotation, their broker IdPs and user links kept), so cutting it falls
# back to break-glass.
step "S&V's own Keycloak signing people in"
K patch idc "$IC" -n "$NS" --type merge -p "$(echo "$ORIG" | jq -c --argjson k "$KI" '{spec: {failback: "Automatic",
  tiers: (.tiers | to_entries | map(if .key < $k then .value.drain = true else . end | .value))}}')" >/dev/null
expect '^[0-9]+s$' "$T becomes active" "$(within 20 is_active "$T")"
expect "^$T\$" "broker redirector -> $T" "$(redirector)"
expect "^https://login\.$SV_DOMAIN/realms/workforce/protocol/openid-connect/auth\?.*broker%2F$T%2Fendpoint" \
  "browser sign-in goes straight to S&V's own Keycloak, back to /broker/$T/endpoint" "$(login_lands)"
tok=$(sso_token bob bob-demo | jq -r '.access_token // empty') || tok=""   # no token: the check below fails
expect "^$BOB_ID $T\$" "Bob signs in there: his S&V user, session from $T" \
  "$(echo "$tok" | jq -rR 'split(".")[1] | gsub("-";"+") | gsub("_";"/") | @base64d | fromjson | "\(.sub) \(.idp)"' 2>/dev/null)"
unset tok

step "Kill switch: S&V's own Keycloak off the network (DENY policy in $TNS)"
cut_at=$(date -u +%s)
partition "$T" "$TNS"
expect '^[0-9]+s$' "probes see it: fails over to $LOCAL" "$(within 25 is_active "$LOCAL")"
expect '^false' "IdP unhealthy" "$(tier "$T" healthy) $(tier "$T" reason)"
expect '^none$' "redirector cleared" "$(redirector)"
expect '^S&V login form$' "browser sign-in shows the broker's own form (break-glass)" "$(login_lands)"
expect "^1\$" "Event FailoverActivated" "$(events FailoverActivated "$cut_at")"
healed_at=$(date -u +%s)
heal "$T" "$TNS"
expect '^[0-9]+s$' "healed: fails back to $T (Automatic)" "$(within 35 is_active "$T")"
expect "^1\$" "Event Failback" "$(events Failback "$healed_at")"

step "Rules take effect live"
K patch idc "$IC" -n "$NS" --type merge -p '{"spec":{"failback":"Manual"}}' >/dev/null
partition "$T" "$TNS"; within 25 is_active "$LOCAL" >/dev/null; heal "$T" "$TNS"
within 35 sh -c "[ \"\$(kubectl --context $KCTX get idc $IC -n $NS -o json | jq -r '.status.tiers[]|select(.name==\"$T\").healthy')\" = true ]" >/dev/null; sleep 6
expect "^$LOCAL\$" "failback Manual: healthy again, but stays on $LOCAL" "$(active)"
K patch idc "$IC" -n "$NS" --type merge -p '{"spec":{"failback":"Automatic"}}' >/dev/null
expect '^[0-9]+s$' "failback Automatic: moves back up" "$(within 10 is_active "$T")"
K patch idc "$IC" -n "$NS" --type json -p "[{\"op\":\"add\",\"path\":\"/spec/tiers/$KI/drain\",\"value\":true}]" >/dev/null
expect '^[0-9]+s$' "drain: out of rotation" "$(within 10 is_active "$LOCAL")"
K patch idc "$IC" -n "$NS" --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/tiers/$KI/drain\",\"value\":false}]" >/dev/null
within 10 is_active "$T" >/dev/null
K patch idc "$IC" -n "$NS" --type json -p "[{\"op\":\"add\",\"path\":\"/spec/tiers/$KI/failoverWhen/latencyAboveMs\",\"value\":1}]" >/dev/null
expect '^[0-9]+s SlowResponse$' "failoverWhen latencyAboveMs=1: slow counts, fails over" "$(within 25 is_active "$LOCAL") $(tier "$T" reason)"
K patch idc "$IC" -n "$NS" --type json -p "[{\"op\":\"remove\",\"path\":\"/spec/tiers/$KI/failoverWhen/latencyAboveMs\"}]" >/dev/null
expect '^[0-9]+s$' "rule removed: back to $T" "$(within 35 is_active "$T")"

step "Directory sync: the primary IdP into S&V's profile, out to the failovers"
# On the IdPs as installed: S&V's own Keycloak (keycloak) has a directory.
# department is added to S&V's profile and paired with keycloak's only (any
# other IdP's department mapping is set aside until exit); then the sync
# writes it there as a failover, and reads it from there as the primary.
A=department CJ="$IC-profile-sync" W=verify-$(openssl rand -hex 3)
K patch idc "$IC" -n "$NS" --type merge -p "$(echo "$ORIG" | jq -c --arg a "$A" '{spec: {
  profile: {attributes: ((.profile.attributes // []) + [{name: $a, displayName: "Department"}] | unique_by(.name))},
  sync: {schedule: "0 3 * * *", suspend: true},
  tiers: (.tiers | map(if .name == "keycloak" then .attributes = ((.attributes // []) + [{attribute: $a, path: $a}] | unique_by(.attribute))
    elif .attributes then .attributes |= map(select(.attribute != $a)) else . end))}}')" >/dev/null
profile_has() { kcadm /users/profile | jq -e --arg a "$A" '.attributes[] | select(.name==$a) | .permissions.edit == ["admin"]'; }
expect '^[0-9]+s$' "S&V's profile: $A added, users view only" "$(within 20 profile_has)"
expect '^True$' "ProfileApplied" "$(K get idc "$IC" -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="ProfileApplied")].status}')"
cj_ready() { K get cronjob "$CJ" -n "$NS" >/dev/null; }
expect '^[0-9]+s$' "CronJob $CJ" "$(within 15 cj_ready)"
cj_scheduled() { [ "$(K get cronjob "$CJ" -n "$NS" -o jsonpath='{.spec.schedule}')" = "0 3 * * *" ]; }
within 15 cj_scheduled >/dev/null   # the controller applies spec.sync on its next pass
expect '^0 3 \* \* \* Etc/UTC true Forbid IdentityContinuity continuity-sync$' "schedule UTC, suspended, no overlap, owned by the instance, runs as continuity-sync" \
  "$(K get cronjob "$CJ" -n "$NS" -o jsonpath='{.spec.schedule} {.spec.timeZone} {.spec.suspend} {.spec.concurrencyPolicy} {.metadata.ownerReferences[0].kind} {.spec.jobTemplate.spec.template.spec.serviceAccountName}')"
can() { K auth can-i get "secret/$2" -n "$NS" --as="system:serviceaccount:$NS:$1" 2>/dev/null; }
expect '^no no yes yes$' "the sync reads only its Secrets and the directories': not the controller's or an IdP's client secret" \
  "$(can continuity-sync continuity-controller) $(can continuity-sync upstream-auth0) $(can continuity-sync continuity-sync) $(can continuity-sync directory-keycloak)"
expect '^no$' "the controller can't read a directory's credentials" "$(can continuity-controller directory-keycloak)"

# Bob's department at the broker (as the sync's client) and at S&V's own
# Keycloak (as its directory client)
wlp=$(free_port); port_forward sv-workforce keycloak "$wlp" 80
as_client() {  # as_client <base> <realm> <client> <secret> <path> [curl args]
  local t; t=$(printf 'grant_type=client_credentials&client_id=%s&client_secret=%s' "$3" "$4" \
    | curl -s "$1/realms/$2/protocol/openid-connect/token" --data @- | jq -r .access_token)
  with_bearer "$t" curl -s "$1/admin/realms/$2$5" "${@:6}"
}
broker() { as_client "$KC" sterling-vance continuity-sync "$(lab_secret_get SV_CONTINUITY_SYNC_CLIENT_SECRET)" "$@"; }
workforce() { as_client "http://127.0.0.1:$wlp" workforce continuity-directory "$(lab_secret_get SV_WORKFORCE_DIRECTORY_SECRET)" "$@"; }
dept_of() { "$1" "/users/$2" | jq -r --arg a "$A" '.attributes[$a][0] // ""'; }
set_dept() {  # set_dept broker|workforce <user id> <value>: the whole representation back
  local f="$TMPD/user-$1.json"   # not stdin: the token goes to curl on stdin
  "$1" "/users/$2" | jq -c --arg a "$A" --arg v "$3" '.attributes[$a] = (if $v == "" then [] else [$v] end)' >"$f"
  "$1" "/users/$2" -X PUT -H 'content-type: application/json' --data "@$f" -o /dev/null -w '%{http_code}'
  rm -f "$f"
}
BOB_WF=$(workforce "/users?username=bob&exact=true" | jq -r '.[0].id // empty') || BOB_WF=""
[ -n "$BOB_WF" ] || die "no user bob in S&V's own Keycloak (realm workforce), as its directory client sees it"
# Bob's department as it was, at both, put back on exit
DEPT_BROKER=$(dept_of broker "$BOB_ID") && DEPT_WF=$(dept_of workforce "$BOB_WF") || die "could not read Bob's $A at the broker and S&V's own Keycloak"
on_exit "set_dept broker $BOB_ID $(printf %q "$DEPT_BROKER") >/dev/null 2>&1; set_dept workforce $BOB_WF $(printf %q "$DEPT_WF") >/dev/null 2>&1"
run_sync() {  # run_sync [args]: one run of the CronJob's job, its pod's termination message
  local j; j="$CJ-verify-$(openssl rand -hex 3)"
  K get cronjob "$CJ" -n "$NS" -o json | jq --arg n "$j" --argjson extra "$(jq -nc '$ARGS.positional' --args -- "$@")" '{apiVersion: "batch/v1", kind: "Job",
    metadata: {name: $n, labels: (.spec.jobTemplate.metadata.labels + {"continuity.lab.solo.io/verify": "true"})},
    spec: (.spec.jobTemplate.spec | .template.spec.containers[0].args += $extra)}' | K apply -n "$NS" -f - >/dev/null
  done_() { [ -n "$(K get job "$j" -n "$NS" -o jsonpath='{.status.failed}{.status.succeeded}')" ]; }
  within 120 done_ >/dev/null
  K get pods -n "$NS" -l job-name="$j" -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.message}'
}
sync_status() { idc | jq -r '.status.sync | "updated=\(.updated // 0) written=\(.written // 0) failed=\(.failed // 0) \(.message)"'; }

expect '^204$' "Bob's department set in S&V's profile" "$(set_dept broker "$BOB_ID" "$W-out")"
run_sync >/dev/null
expect "^$W-out\$" "written out to the failover: S&V's own Keycloak has Bob's department" "$(dept_of workforce "$BOB_WF")"
expect 'written=[1-9].* failed=0' "status.sync: failover accounts written, no failures" "$(sync_status)"

K patch idc "$IC" -n "$NS" --type merge -p "$(idc | jq -c '{spec: {tiers: ([.spec.tiers[] | select(.name == "keycloak")] + [.spec.tiers[] | select(.name != "keycloak")])}}')" >/dev/null
expect '^204$' "S&V's own Keycloak made the primary; Bob's department changed there" "$(set_dept workforce "$BOB_WF" "$W-in")"
run_sync >/dev/null
expect "^$W-in\$" "read in from the primary: S&V's profile has it" "$(dept_of broker "$BOB_ID")"
expect 'updated=[1-9].* failed=0' "status.sync: S&V profiles updated, no failures" "$(sync_status)"

expect '"ok":true,"users":[2-9][0-9]*,"attributes":\[[^]]*"firstName"' "Test connection: token, users counted, attribute schema read (no user data)" \
  "$(run_sync --test-tier=keycloak)"

step "An IdP removed and added back at runtime"
K patch idc "$IC" -n "$NS" --type merge -p "$(echo "$ORIG" | jq -c '{spec: (. | .tiers |= map(select(.name != "keycloak")))}')" >/dev/null
expect '^[0-9]+s$' "$T out of the chain: its broker IdP deleted" "$(within 15 idp_gone "$T")"
K patch idc "$IC" -n "$NS" --type merge -p "{\"spec\": $ORIG}" >/dev/null
idp_back() { ! idp_gone "$T"; }
expect '^[0-9]+s$' "$T back in the chain: its broker IdP created again" "$(within 20 idp_back)"
expect "^[0-9]+s\$" "active returns to $BASE" "$(within 30 is_active "$BASE")"
if [ "$(echo "$ORIG" | jq -r '.sync')" = null ]; then
  if echo "$ORIG" | jq -e '.tiers | any(.directory)' >/dev/null; then
    cj_suspended() { [ "$(K get cronjob "$CJ" -n "$NS" -o jsonpath='{.spec.suspend}')" = true ]; }
    expect '^[0-9]+s$' "CronJob $CJ kept for the directories as installed, never on a schedule" "$(within 15 cj_suspended)"
  else
    expect '^[0-9]+s$' "CronJob $CJ deleted" "$(within 15 sh -c "! kubectl --context $KCTX get cronjob $CJ -n $NS")"
  fi
fi
if [ "$(echo "$ORIG" | jq -r '.profile')" = null ]; then
  profile_gone() { ! profile_has; }
  expect '^[0-9]+s$' "profile attribute $A removed" "$(within 20 profile_gone)"
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

echo; [ $fail -eq 0 ] && ok "continuity: $pass/$((pass+fail)) checks passed$([ "$skip" -eq 0 ] || echo ", $skip skipped")" || die "continuity: $fail of $((pass+fail)) checks failed"
