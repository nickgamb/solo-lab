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

# The chain (ENTERPRISE_IDP) is checked before anything is built or changed:
# known IdPs only, each once, at least one with an issuer.
. "$LAB_ROOT/scripts/idp.sh"
CHAIN=$(idp_chain) || exit 1
# has_directory <name>: whether the directory sync reaches the IdP's users. A
# directory takes its type, URL and both halves of its client's credentials,
# or none of the credentials.
has_directory() {
  local N; N=$(echo "$1" | tr '[:lower:]' '[:upper:]')
  [ -n "$(_idp_var "$1" DIRECTORY_TYPE)" ] || return 1
  if [ -z "$(_idp_var "$1" DIRECTORY_CLIENT_ID)$(_idp_var "$1" DIRECTORY_CLIENT_SECRET)" ]; then return 1; fi
  [ -n "$(_idp_var "$1" DIRECTORY_CLIENT_ID)" ] && [ -n "$(_idp_var "$1" DIRECTORY_CLIENT_SECRET)" ] \
    || die "$1 as a directory needs both ${N}_DIRECTORY_CLIENT_ID and ${N}_DIRECTORY_CLIENT_SECRET in .env (or neither)"
  [ -n "$(_idp_var "$1" DIRECTORY_URL)" ] || die "$1 as a directory needs ${N}_DIRECTORY_URL in .env"
}
for n in $CHAIN; do has_directory "$n" || true; done

step "continuity-controller image (local registry)"
# tagged by source content, so a code change is a new image and a rollout
tag=$(src_hash "$APP")
export CONTINUITY_IMAGE="localhost:$LAB_REGISTRY_PORT/lab/continuity-controller:$tag"
docker image inspect "$CONTINUITY_IMAGE" >/dev/null 2>&1 || docker build -q -t "$CONTINUITY_IMAGE" "$APP" >/dev/null \
  || die "building $CONTINUITY_IMAGE failed"
docker push -q "$CONTINUITY_IMAGE" >/dev/null || die "pushing $CONTINUITY_IMAGE failed"
ok "$CONTINUITY_IMAGE (unit tests ran in the build)"

step "Credentials"
# an IdP's or directory's credentials: the only Secrets the Observatory may
# replace (platform/90-observatory/admission.yaml); the controller's and the
# sync's own clients stay out of its reach
credentials() { K label secret "$1" -n sv-identity continuity.lab.solo.io/credentials=true --overwrite >/dev/null; }
# the controller's own realm client (defined in realm-sterling-vance.json)
secret_apply sv-identity continuity-controller \
  client-id=continuity-controller client-secret="$(lab_secret SV_CONTINUITY_CLIENT_SECRET)"
# the scheduled profile sync's realm client (users' profile attributes only)
secret_apply sv-identity continuity-sync \
  client-id=continuity-sync client-secret="$(lab_secret SV_CONTINUITY_SYNC_CLIENT_SECRET)"
for n in $CHAIN; do
  N=$(echo "$n" | tr '[:lower:]' '[:upper:]')
  # the directory sync's client at the IdP's directory
  if has_directory "$n"; then
    secret_apply sv-identity "directory-$n" \
      client-id="$(_idp_var "$n" DIRECTORY_CLIENT_ID)" client-secret="$(idp_secret "$n" DIRECTORY_CLIENT_SECRET)"
    credentials "directory-$n"
  fi
  if [ -z "$(_idp_var "$n" CLIENT_ID)" ]; then
    warn "$n tier is NotConfigured until ${N}_CLIENT_ID is in .env (then re-run this layer)"
  elif [ "$(idp_client_auth "$n")" = private_key_jwt ]; then
    K delete secret "upstream-$n" -n sv-identity --ignore-not-found >/dev/null
    ok "$n: private_key_jwt with S&V's broker key (make xaa-keys for its JWKS)"
  else
    secret_apply sv-identity "upstream-$n" \
      client-id="$(_idp_var "$n" CLIENT_ID)" client-secret="$(idp_secret "$n" CLIENT_SECRET)"
    credentials "upstream-$n"
    ok "$n: client_secret_post (sv-identity/upstream-$n)"
  fi
done
ok "sv-identity/continuity-controller  (chain: $(echo "$CHAIN" | sed 's/ / -> /g'))"

step "Egress waypoint (sv-egress)"
apply_tmpl "$D/egress.yaml"
wait_for "egress waypoint Programmed" 40 3 K wait -n sv-egress gateway/egress-waypoint --for=condition=Programmed --timeout=2s
ok "external upstreams leave through sv-egress/egress-waypoint"

step "IdentityContinuity and WorkloadProfile CRDs + controller (sv-identity)"
K apply --server-side -f "$APP/config/crd" >/dev/null
K wait crd/identitycontinuities.continuity.lab.solo.io crd/workloadprofiles.continuity.lab.solo.io --for=condition=Established --timeout=60s >/dev/null
# each IdP's client and directory Secrets, by name: the controller reads the
# first, the sync the second
CONTINUITY_UPSTREAM_SECRETS=$(for n in $CHAIN; do printf ', upstream-%s' "$n"; done)
CONTINUITY_DIRECTORY_SECRETS=$(for n in $CHAIN; do printf ', directory-%s' "$n"; done)
export CONTINUITY_UPSTREAM_SECRETS CONTINUITY_DIRECTORY_SECRETS
apply_tmpl "$D/controller.yaml"
# the assurance gate: the same image, its gate command
apply_tmpl "$D/assurance-gate.yaml"
# the fabric's routing: sign-ins pass the firm's gateway, which the
# controller tells where each one goes (spec.routing)
AGW_POLICY_RESOURCE=$(echo "$AGW_POLICY_KIND" | tr '[:upper:]' '[:lower:]' | sed 's/y$/ies/') apply_tmpl "$D/routing.yaml"
# partitions are read in the egress namespace only; a lab from before that
# still has the cluster-wide grant
K delete clusterrolebinding,clusterrole continuity-controller-partitions --ignore-not-found >/dev/null
rollout sv-identity deploy/continuity-controller
rollout sv-identity deploy/assurance-gate
# The tiers follow ENTERPRISE_IDP: which IdPs and in what order, then the
# broker's break-glass accounts (platform admins only), each from its NAME_*
# settings (config/lab.env). Each existing tier keeps its failover rules,
# attribute mappings, directory and assurance (the operators' and the
# Observatory's); issuers and token settings follow .env.
# What each IdP's sign-ins prove (NAME_ASSURANCE, NIST 800-63B levels by the
# acr or amr it asserts): a sign-in asserting none of them is its default,
# AAL1. An IdP whose policy for S&V's client always takes a second factor says
# so in config/continuity.local.yaml (or the Observatory) with default: AAL2.
# A directory's attribute mapping follows its type: where its records keep
# the email and names.
DIRECTORY_ATTRIBUTES='{
  "auth0": [{"attribute": "email", "path": "email"}, {"attribute": "firstName", "path": "given_name"}, {"attribute": "lastName", "path": "family_name"}],
  "keycloak": [{"attribute": "email", "path": "email"}, {"attribute": "firstName", "path": "firstName"}, {"attribute": "lastName", "path": "lastName"}],
  "scim": [{"attribute": "email", "path": "emails[primary eq true].value"}, {"attribute": "firstName", "path": "name.givenName"}, {"attribute": "lastName", "path": "name.familyName"}]}'
desired=$({ for n in $CHAIN; do
  dir=null
  if has_directory "$n"; then
    dir=$(jq -nc --arg n "$n" --arg type "$(_idp_var "$n" DIRECTORY_TYPE)" --arg url "$(_idp_var "$n" DIRECTORY_URL)" \
      --arg scopes "$(_idp_var "$n" DIRECTORY_SCOPES)" --arg aud "$(_idp_var "$n" DIRECTORY_AUDIENCE)" --argjson attrs "$DIRECTORY_ATTRIBUTES" '
      {directory: ({type: $type, url: $url, credentialsRef: {name: "directory-\($n)"}}
        + (if $scopes != "" then {scopes: ($scopes | split(" ") | map(select(. != "")))} else {} end)
        + (if $aud != "" then {audience: $aud} else {} end)),
       attributes: ($attrs[$type] // [{attribute: "email", path: "email"}])}')
  fi
  assurance=$(idp_setting "$n" ASSURANCE '{"default": "AAL1"}')
  echo "$assurance" | jq -e . >/dev/null 2>&1 || die "$(echo "$n" | tr '[:lower:]' '[:upper:]')_ASSURANCE isn't JSON"
  jq -nc --arg n "$n" --arg iss "$(_idp_var "$n" ISSUER)" --arg cid "$(_idp_var "$n" CLIENT_ID)" \
    --arg auth "$(idp_client_auth "$n")" --argjson dir "$dir" --argjson assurance "$assurance" \
    --arg gclaim "$(idp_setting "$n" GROUPS_CLAIM groups)" --arg display "$(idp_setting "$n" DISPLAY_NAME "$n")" '{name: $n,
    displayName: $display, type: "oidc",
    oidc: ({issuer: $iss, clientID: $cid, clientAuth: $auth}
      + (if $auth == "private_key_jwt" then {clientAssertionSigningAlg: "PS256"} else {clientSecretRef: {name: "upstream-\($n)"}} end)),
    failoverWhen: {unreachable: true, serverError: true, invalidDiscovery: true, latencyAboveMs: 1500},
    assurance: $assurance,
    groups: {claim: $gclaim}}
    + ($dir // {})'
done
jq -nc '{name: "break-glass", displayName: "Platform admins (break-glass)", type: "local", assurance: {default: "AAL1"}}'
} | jq -sc .)
# This lab's own profile and mappings, beyond the defaults (gitignored;
# config/continuity.example.yaml shows the format): S&V profile attributes,
# each IdP's attribute mappings, a sync schedule. A fresh install takes them
# as they are; an existing one gains what it doesn't have yet, so what
# operators changed since stays.
LOCAL="$LAB_ROOT/config/continuity.local.yaml"
local_json='{}'
if [ -f "$LOCAL" ]; then
  local_json=$(yq -o json -I0 '.' "$LOCAL") || die "$LOCAL: not valid YAML"
  ok "local profile and mappings: config/continuity.local.yaml"
fi
# add_local <tiers>: each tier with the local mappings it doesn't have yet
add_local() {
  jq -c --argjson l "$local_json" 'map(. as $t | (($l.tiers // {})[$t.name].attributes // []) as $a
    | if ($a | length) == 0 then . else
        .attributes = ((.attributes // []) + [$a[] | select(.attribute as $n | ([($t.attributes // [])[].attribute] | index($n)) == null)])
      end)' <<<"$1"
}
# local_assurance <tiers>: each IdP's assurance from the local file, if it has one
local_assurance() {
  jq -c --argjson l "$local_json" 'map(. as $t | (($l.tiers // {})[$t.name].assurance) as $a | if $a then .assurance = $a else . end)' <<<"$1"
}
desired=$(local_assurance "$desired")
# the shape's groups and the workforce's email domains (config/lab.env)
SHAPE=$(jq -nc --arg g "$SV_GROUPS" --arg d "$SV_EMAIL_DOMAINS" '{groups: ($g | split(",") | map(select(. != ""))), domains: ($d | split(",") | map(select(. != "")))}')
if ! K get identitycontinuity sterling-vance -n sv-identity >/dev/null 2>&1; then
  desired=$(add_local "$desired")
  render "$D/identitycontinuity.yaml" | T="$desired" L="$local_json" yq '.spec.tiers = env(T)
    | (env(L) | .profile) as $p | (env(L) | .sync) as $s
    | (select($p != null) | .spec.profile) = $p | (select($s != null) | .spec.sync) = $s' \
    | G="$SHAPE" yq '.spec.profile = ((.spec.profile // {}) * env(G))' | K apply -f - >/dev/null
else
  cur=$(K get idc sterling-vance -n sv-identity -o json | jq -c '.spec.tiers')
  # an existing tier keeps what operators set (display name, enabled, failover
  # rules, attribute mappings, directory); .env sets its type, issuer, secret
  # and token settings
  merged=$(jq -nc --argjson d "$desired" --argjson c "$cur" '$d | map(. as $t
    | ([$c[] | select(.name == $t.name and .type == $t.type)][0]) as $old
    | if $old == null then $t
      else ($old * ($t | del(.displayName, .failoverWhen) | if $old.directory then del(.directory) else . end
          | if $old.attributes then del(.attributes) else . end | if $old.assurance then del(.assurance) else . end
          | if $old.groups then del(.groups) else . end))
        | if .oidc.clientAuth == "private_key_jwt" then del(.oidc.clientSecretRef) else . end
        # the broker vouches for everyone, so no upstream tokens are kept (labs from before)
        | del(.oidc.storeTokens) end)')
  # every S&V caller of an upstream goes through the egress waypoint
  egress=$(render "$D/identitycontinuity.yaml" | yq -o json -I0 '.spec.egress')
  if [ "$(K get idc sterling-vance -n sv-identity -o json | jq -cS '.spec.egress | del(.internalDomains)')" != "$(echo "$egress" | jq -cS .)" ]; then
    # a key the spec no longer has (exportTo, labs from before) is removed
    K patch idc sterling-vance -n sv-identity --type merge -p "{\"spec\":{\"egress\":$(echo "$egress" | jq -c '{exportTo: null} + .')}}" >/dev/null
    ok "egress: $(echo "$egress" | jq -c .)"
  fi
  merged=$(add_local "$merged")
  # local profile attributes the instance doesn't have yet
  prof=$(K get idc sterling-vance -n sv-identity -o json | jq -c --argjson l "$local_json" '(.spec.profile.attributes // []) as $c
    | ($l.profile.attributes // []) as $a | [$a[] | select(.name as $n | ([$c[].name] | index($n)) == null)] as $new
    | if ($new | length) == 0 then empty else {spec: {profile: {attributes: ($c + $new)}}} end')
  if [ -n "$prof" ]; then
    K patch idc sterling-vance -n sv-identity --type merge -p "$prof" >/dev/null
    ok "profile: $(echo "$prof" | jq -r '[.spec.profile.attributes[].name] | join(", ")')"
  fi
  # the shape's groups and the workforce's domains, unless operators set them
  shape=$(K get idc sterling-vance -n sv-identity -o json | jq -c --argjson s "$SHAPE" '(.spec.profile // {}) as $p
    | [if $p.groups == null then {groups: $s.groups} else empty end, if $p.domains == null then {domains: $s.domains} else empty end]
    | if length == 0 then empty else {spec: {profile: add}} end')
  if [ -n "$shape" ]; then
    K patch idc sterling-vance -n sv-identity --type merge -p "$shape" >/dev/null
    ok "profile: $(echo "$shape" | jq -c .spec.profile)"
  fi
  if [ "$(echo "$cur" | jq -cS .)" != "$(echo "$merged" | jq -cS .)" ]; then
    K patch idc sterling-vance -n sv-identity --type merge -p "{\"spec\":{\"tiers\":$merged}}" >/dev/null
    ok "tiers: $(echo "$cur" | jq -r 'map(.name) | join(" -> ")') => $(echo "$merged" | jq -r 'map(.name) | join(" -> ")')"
  fi
fi
# the routing policy's gateway follows the edition; the rules stay the
# identity team's (the defaults only where there are none yet)
want=$(render "$D/identitycontinuity.yaml" | yq -o json -I0 '.spec.routing')
routing=$(K get idc sterling-vance -n sv-identity -o json | jq -c --argjson w "$want" '(.spec.routing // {}) as $r
  | {policy: $w.policy, rules: ($r.rules // $w.rules)} as $n | if $n == $r then empty else {spec: {routing: $n}} end')
if [ -n "$routing" ]; then
  K patch idc sterling-vance -n sv-identity --type merge -p "$routing" >/dev/null
  ok "routing: $(echo "$routing" | jq -r '[.spec.routing.rules[] | "\(.name) -> \(.idps | join(", "))"] | join("; ")')"
fi
# a latency rule at or above the probe timeout can never fire (the probe times
# out first): earlier versions installed one, so bring it under the timeout
fix=$(K get idc sterling-vance -n sv-identity -o json | jq -c '((.spec.health.timeoutSeconds // 2) * 1000) as $t
  | [.spec.tiers | to_entries[] | select((.value.failoverWhen.latencyAboveMs // 0) >= $t)
     | {op: "replace", path: "/spec/tiers/\(.key)/failoverWhen/latencyAboveMs", value: ($t * 3 / 4 | floor)}]')
if [ "$fix" != "[]" ]; then K patch idc sterling-vance -n sv-identity --type json -p "$fix" >/dev/null; ok "latencyAboveMs brought under the probe timeout"; fi
wait_for "sterling-vance continuity Ready" 30 2 \
  sh -c "kubectl --context $KCTX get idc sterling-vance -n sv-identity -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' | grep -q True"
ok "active IdP: $(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}')  (kubectl get idc -n sv-identity)"
# The directory sync makes the broker's accounts from the primary IdP and maps
# everyone's groups, so it runs on a schedule (unless operators set their own)
# and once now: the demos sign people in next.
if [ -z "$(K get idc sterling-vance -n sv-identity -o jsonpath='{.spec.sync}')" ]; then
  K patch idc sterling-vance -n sv-identity --type merge -p "{\"spec\":{\"sync\":{\"schedule\":\"$SV_SYNC_SCHEDULE\"}}}" >/dev/null
fi
# who leaves the primary leaves the broker (SV_SYNC_REMOVE_MISSING; the
# Observatory's Directory sync tab switches it at runtime)
case "$SV_SYNC_REMOVE_MISSING" in true|false) ;; *) die "SV_SYNC_REMOVE_MISSING: true or false" ;; esac
K patch idc sterling-vance -n sv-identity --type merge -p "{\"spec\":{\"sync\":{\"removeMissing\":$SV_SYNC_REMOVE_MISSING}}}" >/dev/null
# the controller's own image: a CronJob still on the previous one is updated on its next pass
img=$(K get deploy continuity-controller -n sv-identity -o jsonpath='{.spec.template.spec.containers[0].image}')
wait_for "the directory sync's CronJob, on $img" 60 2 \
  sh -c "kubectl --context $KCTX get cronjob sterling-vance-profile-sync -n sv-identity -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}' | grep -qx '$img'"
job="sterling-vance-profile-sync-install-$(date -u +%Y%m%d%H%M%S)"
K create job "$job" -n sv-identity --from=cronjob/sterling-vance-profile-sync >/dev/null
if K wait job "$job" -n sv-identity --for=condition=Complete --timeout=180s >/dev/null 2>&1; then
  ok "directory sync: $(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.sync.message}')"
else
  warn "directory sync: $(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.sync.message}') (kubectl logs -n sv-identity job/$job)"
fi
