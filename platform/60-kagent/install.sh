#!/usr/bin/env bash
# kagent + kmcp (bundled subchart), wired to the platform:
#   model   -> agentgateway (no provider key in kagent, ever)
#   runtime -> pods, or Agent Substrate for SandboxAgents
#   traces  -> otel-collector
#   oss:        kagent 0.10.2
#   enterprise: kagent-enterprise (see docs/ENTERPRISE.md; two tracks)
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
ED=$KAGENT_EDITION

[ "$ED" = oss ] && lab_image kagent-dev/kagent/controller:0.10.2-lab.3 tools/kagent/build.sh
step "kagent $KAGENT_VERSION ($ED)"
# kagent's OpenAI client insists on a key. agentgateway ignores it and injects
# the real provider credential, so this placeholder is all kagent ever holds.
K create secret generic kagent-llm -n kagent --from-literal=API_KEY=via-agentgateway \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# The ops worker pool's identity (the chart's WorkerPool runs as it)
K create serviceaccount kagent-ops -n kagent --dry-run=client -o yaml | K apply -f - >/dev/null
helm_up kagent-crds "$KAGENT_CRDS_CHART" "$KAGENT_VERSION" kagent
values_for "$D" values "$ED"
HELM_TIMEOUT=15m helm_up kagent "$KAGENT_CHART" "$KAGENT_VERSION" kagent ${VALS[@]+"${VALS[@]}"}
rollout kagent deploy/kagent-controller deploy/kagent-ui

step "Ops agents on Agent Substrate (kagent-ops)"
# The chart's built-in agents, rendered from the same chart and made
# SandboxAgents on the ops pool. A SandboxAgent can't share a name with an
# Agent (config Secret, session ids), so a helm-made Agent must be gone first.
OPS="k8s-agent kgateway-agent istio-agent helm-agent promql-agent"
for a in $OPS; do K wait "agent/$a" -n kagent --for=delete --timeout=120s >/dev/null 2>&1 || true; done
on=$(printf ',%s.enabled=true' $OPS)
H template kagent "$KAGENT_CHART" --version "$KAGENT_VERSION" -n kagent ${VALS[@]+"${VALS[@]}"} --set "${on#,}" 2>"$LAB_STATE/helm-ops-agents.log" \
  | yq 'select(.kind == "Agent") | .kind = "SandboxAgent" | .spec.substrate.workerPoolRef.name = "kagent-ops"
    | del(.metadata.labels."helm.sh/chart") | .metadata.labels."app.kubernetes.io/managed-by" = "solo-lab"' \
  | K apply -f - >/dev/null
for a in $OPS; do wait_for "SandboxAgent $a Ready" 120 5 K wait "sandboxagent/$a" -n kagent --for=condition=Ready --timeout=2s; done
ok "$OPS"

step "Edge SSO: https://kagent.${SV_DOMAIN}"
apply_tmpl "$D/edge-sso.yaml"
ok "kgateway OAuth2 -> S&V Keycloak; access token forwarded to kagent"
