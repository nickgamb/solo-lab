#!/usr/bin/env bash
# The Observatory's reach: what its signed-in admins may change (policy,
# routing, identity continuity) and what they may not (workloads, RBAC,
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
expect '^no$' "may not read a Secret" "$(can get secrets -n sv-identity)"
expect '^no$' "may not list Secrets anywhere" "$(can list secrets -A)"
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

step "Its own account: one Role, to grant the sync a directory's Secret"
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

echo; [ $fail -eq 0 ] && ok "observatory: $pass/$((pass+fail)) checks passed" || die "observatory: $fail of $((pass+fail)) checks failed"
