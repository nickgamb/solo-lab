#!/usr/bin/env bash
# The Observatory's reach: what its signed-in admins may change (policy,
# routing, identity and model continuity) and what they may not (workloads, RBAC,
# admission, reading Secrets), checked as the identity the Observatory
# impersonates. Writes are server-side dry runs: nothing is changed.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster
AS=(--as=observatory:admin --as-group=observatory:observatory-admins)
TMPD=$(umask 077; mktemp -d); on_exit "rm -rf $TMPD"
pass=0 fail=0
res() { if [ "$1" = ok ]; then ok "$2"; pass=$((pass+1)); else warn "$2"; echo "      got: ${3:0:300}"; fail=$((fail+1)); fi; }
expect() { echo "$3" | grep -qE "$1" && res ok "$2" || res no "$2" "$3"; }
can() { K auth can-i "$@" "${AS[@]}" 2>/dev/null; }

step "RBAC: policy and routing, not what runs"
expect '^yes$' "may change an AuthorizationPolicy" "$(can patch authorizationpolicies.security.istio.io -n sv-egress)"
expect '^yes$' "may change an HTTPRoute" "$(can patch httproutes.gateway.networking.k8s.io -n agentgateway-system)"
expect '^yes$' "may change an IdentityContinuity" "$(can update identitycontinuities.continuity.lab.solo.io -n sv-identity)"
expect '^no$' "may not change its status (the active IdP is the controller's)" "$(can update identitycontinuities.continuity.lab.solo.io --subresource=status -n sv-identity)"
expect '^yes yes yes$' "may add, change and remove a resource's assurance rules (WorkloadProfile)" \
  "$(can create workloadprofiles.continuity.lab.solo.io -n sv-identity) $(can update workloadprofiles.continuity.lab.solo.io -n sv-identity) $(can delete workloadprofiles.continuity.lab.solo.io -n sv-identity)"
expect '^yes yes$' "may let a gateway policy ask the assurance gate (a ReferenceGrant and an AuthorizationPolicy beside it)" \
  "$(can create referencegrants.gateway.networking.k8s.io -n sv-identity) $(can create authorizationpolicies.security.istio.io -n sv-identity)"
expect '^no$' "may not change their status (whether they're met is the controller's)" "$(can update workloadprofiles.continuity.lab.solo.io --subresource=status -n sv-identity)"
expect '^no$' "may not read a Secret" "$(can get secrets -n sv-identity)"
expect '^no$' "may not list Secrets anywhere" "$(can list secrets -A)"
expect '^yes$' "may change the model chain (AI gateway backends)" "$(can update agentgatewaybackends.agentgateway.dev -n agentgateway-system)"
expect '^no$' "may not read a model provider's key" "$(can get secrets -n agentgateway-system)"
expect '^no$' "may not change a Deployment" "$(can patch deployments.apps -n sv-agents)"
expect '^no$' "may not create a pod" "$(can create pods -n kagent)"
expect '^no$' "may not exec into a pod" "$(can create pods/exec -n sv-identity)"
expect '^no$' "may not change an agent" "$(can patch agents.kagent.dev -n sv-agents)"
expect '^no$' "may not change a ConfigMap (realm imports)" "$(can patch configmaps -n sv-identity)"
expect '^no$' "may not change gateway parameters (proxy images)" "$(can patch agentgatewayparameters.agentgateway.dev -n agentgateway-system)"
expect '^no$' "may not grant itself a role" "$(can create clusterrolebindings)"
expect '^no$' "may not change admission policy" "$(can patch validatingadmissionpolicies.admissionregistration.k8s.io)"
expect '^no$' "may not create a Job outside identity continuity" "$(can create jobs -n kagent)"

# raw requests as the admin, through a local proxy: kubectl's own write
# commands read the object first, which the admin may not do for a Secret
port=$(free_port); K proxy --port="$port" >/dev/null 2>&1 & proxy=$!; on_exit "kill $proxy 2>/dev/null"
wait_for "kubectl proxy" 10 1 curl -sf -o /dev/null "http://127.0.0.1:$port/api"
as_admin() {  # as_admin <method> <path> <json>: the API server's status code and message, as the admin, dry run
  curl -s -X "$1" "http://127.0.0.1:$port$2?dryRun=All&fieldManager=observatory" -H 'Content-Type: application/json' \
    -H 'Impersonate-User: observatory:admin' -H 'Impersonate-Group: observatory:observatory-admins' --data "$3" \
    | jq -r 'if .kind == "Status" then "\(.code) \(.message)" else "201 \(.kind)" end'
}
secret() {  # secret <name> <type> <labelled>: a Secret document
  jq -nc --arg n "$1" --arg t "$2" --argjson l "$3" '{apiVersion: "v1", kind: "Secret", type: $t,
    metadata: ({name: $n, namespace: "sv-identity"} + (if $l then {labels: {"continuity.lab.solo.io/credentials": "true"}} else {} end)),
    stringData: {"client-secret": "verify"}}'
}

step "Admission: credentials only, and only the profile sync"
expect '^201 ' "may write an IdP's credentials (labelled, Opaque)" \
  "$(as_admin POST /api/v1/namespaces/sv-identity/secrets "$(secret verify-credentials Opaque true)")"
expect '^(403|422) .*continuity.lab.solo.io/credentials' "may not write an unlabelled Secret" \
  "$(as_admin POST /api/v1/namespaces/sv-identity/secrets "$(secret verify-other Opaque false)")"
expect '^(403|422) .*Opaque' "may not mint a service-account token" \
  "$(as_admin POST /api/v1/namespaces/sv-identity/secrets "$(jq -c '.metadata.annotations = {"kubernetes.io/service-account.name": "continuity-controller"}' <<<"$(secret verify-token kubernetes.io/service-account-token true)")")"
key=$(K get secret kc-realm-key -n sv-identity -o json | jq -c '{apiVersion, kind, type, metadata: {name: .metadata.name, namespace: .metadata.namespace, labels: {"continuity.lab.solo.io/credentials": "true"}}, stringData: {x: "verify"}}')
expect '^(403|422) .*own' "may not take over the broker's realm key by labelling it" \
  "$(as_admin PUT /api/v1/namespaces/sv-identity/secrets/kc-realm-key "$key")"

CJ=sterling-vance-profile-sync
job() {  # job <jq filter>: a Job from the sync CronJob's template, changed by the filter
  K get cronjob "$CJ" -n sv-identity -o json | jq -c '{apiVersion: "batch/v1", kind: "Job",
    metadata: {generateName: "verify-", namespace: "sv-identity", labels: .spec.jobTemplate.metadata.labels},
    spec: .spec.jobTemplate.spec}' | jq -c "$1"
}
expect '^201 ' "may run the profile sync" "$(as_admin POST /apis/batch/v1/namespaces/sv-identity/jobs "$(job .)")"
expect '^(403|422) .*image and command' "may not run another image as the sync" \
  "$(as_admin POST /apis/batch/v1/namespaces/sv-identity/jobs "$(job '.spec.template.spec.containers[0].image = "busybox"')")"
expect '^(403|422) .*continuity-sync' "may not run the sync as another account" \
  "$(as_admin POST /apis/batch/v1/namespaces/sv-identity/jobs "$(job '.spec.template.spec.serviceAccountName = "continuity-controller"')")"
expect '^(403|422) .*Secret' "may not mount a Secret into the sync" \
  "$(as_admin POST /apis/batch/v1/namespaces/sv-identity/jobs "$(job '.spec.template.spec.volumes += [{name: "s", secret: {secretName: "continuity-controller"}}]')")"

step "Admission: model provider keys, next to the AI gateway only"
model_secret() {  # model_secret <namespace> <name> <labelled>: a model provider's key
  jq -nc --arg ns "$1" --arg n "$2" --argjson l "$3" '{apiVersion: "v1", kind: "Secret", type: "Opaque",
    metadata: ({name: $n, namespace: $ns} + (if $l then {labels: {"lab.solo.io/model-credentials": "true"}} else {} end)),
    stringData: {Authorization: "verify"}}'
}
expect '^201 ' "may write a model provider's key (labelled, in agentgateway-system)" \
  "$(as_admin POST /api/v1/namespaces/agentgateway-system/secrets "$(model_secret agentgateway-system verify-model true)")"
expect '^(403|422) .*lab.solo.io/model-credentials' "may not write an unlabelled Secret there" \
  "$(as_admin POST /api/v1/namespaces/agentgateway-system/secrets "$(model_secret agentgateway-system verify-other false)")"
expect '^(403|422) .*continuity.lab.solo.io/credentials' "may not write a model key in another namespace" \
  "$(as_admin POST /api/v1/namespaces/sv-identity/secrets "$(model_secret sv-identity verify-model true)")"
# a Secret the gateway already has (its license, a helm release), labelled as
# a model key: the old object decides
taken=$(K get secrets -n agentgateway-system -o json | jq -c '[.items[] | select(.metadata.labels["lab.solo.io/model-credentials"] != "true")]
  | (map(select(.metadata.name | test("license"))) + .) | first // empty
  | {apiVersion: "v1", kind: "Secret", type, metadata: {name: .metadata.name, namespace: .metadata.namespace,
     labels: {"lab.solo.io/model-credentials": "true"}}, stringData: {Authorization: "verify"}}')
if [ -n "$taken" ]; then
  name=$(jq -r .metadata.name <<<"$taken")
  expect '^(403|422) .*own' "may not take over $name by labelling it" \
    "$(as_admin PUT "/api/v1/namespaces/agentgateway-system/secrets/$name" "$taken")"
else
  warn "no unlabelled Secret in agentgateway-system to try a takeover on"
fi

step "Admission: the assurance gate fails closed"
# a policy that asks the gate, changed to let requests through when it can't
# answer (as an admin might in the policy editor)
kind=$(echo "$AGW_POLICY_KIND" | tr '[:upper:]' '[:lower:]')
gate=$(K get "$kind" bob-workspace-caller -n sv-mcp -o json 2>/dev/null \
  | jq -c '{apiVersion, kind, metadata: {name: .metadata.name, namespace: .metadata.namespace, resourceVersion: .metadata.resourceVersion}, spec}') || gate=""
if [ -n "$gate" ]; then
  plural=$(echo "$kind" | sed 's/y$/ie/')s
  expect '^201 ' "may change the policy that asks the gate (still FailClosed)" \
    "$(as_admin PUT "/apis/$AGW_POLICY_API/namespaces/sv-mcp/$plural/bob-workspace-caller" "$gate")"
  expect '^(403|422) .*fails closed' "may not make it fail open" \
    "$(as_admin PUT "/apis/$AGW_POLICY_API/namespaces/sv-mcp/$plural/bob-workspace-caller" "$(jq -c '.spec.traffic.extAuth.failureMode = "FailOpen"' <<<"$gate")")"
  # a gate is whatever Service carries the label, not a name: one more, briefly
  K create service clusterip assurance-gate-verify -n sv-identity --tcp=9001 --dry-run=client -o json \
    | jq '.metadata.labels["continuity.lab.solo.io/assurance-gate"] = "true"' | K apply -f - >/dev/null
  on_exit "K delete service assurance-gate-verify -n sv-identity --ignore-not-found >/dev/null 2>&1"
  other=$(jq -c '.spec.traffic.extAuth.backendRef.name = "assurance-gate-verify" | .spec.traffic.extAuth.failureMode = "FailOpen"' <<<"$gate")
  t=0; until r=$(as_admin PUT "/apis/$AGW_POLICY_API/namespaces/sv-mcp/$plural/bob-workspace-caller" "$other"); echo "$r" | grep -q 'assurance-gate-verify fails closed' || [ $t -ge 10 ]; do sleep 1; t=$((t+1)); done
  expect '^(403|422) .*sv-identity/assurance-gate-verify fails closed' "nor a policy asking another gate, found by its label" "$r"
  K delete service assurance-gate-verify -n sv-identity --ignore-not-found >/dev/null
else
  warn "no sv-mcp/bob-workspace-caller policy (make layer-95): gate admission not checked"; fail=$((fail+1))
fi

step "Its own account: one Role, to grant the sync a directory's Secret; the rules' CRDs"
SA=system:serviceaccount:observatory:observatory
as_self() {  # as_self <method> <path> <json>: as the Observatory's own account, dry run
  curl -s -X "$1" "http://127.0.0.1:$port$2?dryRun=All&fieldManager=observatory" -H 'Content-Type: application/json' \
    -H "Impersonate-User: $SA" -H 'Impersonate-Group: system:serviceaccounts' -H 'Impersonate-Group: system:serviceaccounts:observatory' \
    -H 'Impersonate-Group: system:authenticated' --data "$3" | jq -r 'if .kind == "Status" then "\(.code) \(.message)" else "200 \(.kind)" end'
}
role=$(K get role continuity-sync-secrets -n sv-identity -o json | jq -c 'del(.metadata.managedFields)
  | .rules |= map(if (.resources | index("secrets")) then .resourceNames += ["directory-verify"] else . end)')
expect '^200 ' "may add a directory's Secret to the sync's Role (a Secret it can't read itself)" \
  "$(as_self PUT /apis/rbac.authorization.k8s.io/v1/namespaces/sv-identity/roles/continuity-sync-secrets "$role")"
other=$(K get role continuity-controller-secrets -n sv-identity -o json | jq -c 'del(.metadata.managedFields)
  | .rules |= map(if (.resources | index("secrets")) then .resourceNames += ["directory-verify"] else . end)')
expect '^403 ' "may not change any other Role (the controller's)" \
  "$(as_self PUT /apis/rbac.authorization.k8s.io/v1/namespaces/sv-identity/roles/continuity-controller-secrets "$other")"
expect '^no$' "may not read a Secret itself" "$(K auth can-i get secrets -n sv-identity --as="$SA" 2>/dev/null)"
expect '^yes no$' "may read the assurance rules' CRDs (their choices), and no other" \
  "$(K auth can-i get customresourcedefinitions/workloadprofiles.continuity.lab.solo.io --as="$SA" 2>/dev/null) $(K auth can-i get customresourcedefinitions/agents.kagent.dev --as="$SA" 2>/dev/null)"

if [ "$KAGENT_EDITION" = enterprise ] || [ "$AGW_EDITION" = enterprise ] || [ "$ISTIO_EDITION" = enterprise ]; then
  step "Solo UI"
  expect '^true$' "its pods are ready (UI, collectors, ClickHouse)" \
    "$(K get pods -n solo-enterprise -o json | jq '[.items[] | .status.containerStatuses[]?.ready] | length > 0 and all')"
  expect '^200$' "https://kagent.$SV_DOMAIN answers" "$(curl -s -o /dev/null -w '%{http_code}' --cacert "$LAB_CA_DIR/ca.crt" "https://kagent.$SV_DOMAIN/")"
  # S&V's broker takes the sign-in (client kagent-ui, PKCE, its callback) and
  # sends it on to the active IdP; an unknown client or callback is an error page
  expect '^30[23]$' "S&V's broker takes its sign-in (client kagent-ui, PKCE, its callback)" \
    "$(curl -s -o /dev/null -w '%{http_code}' --cacert "$LAB_CA_DIR/ca.crt" "https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/auth?client_id=kagent-ui&redirect_uri=https%3A%2F%2Fkagent.$SV_DOMAIN%2Fcallback&response_type=code&scope=openid&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256&state=verify")"
fi

echo; [ $fail -eq 0 ] && ok "observatory: $pass/$((pass+fail)) checks passed" || die "observatory: $fail of $((pass+fail)) checks failed"
