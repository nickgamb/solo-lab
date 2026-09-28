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

step "kagent $KAGENT_VERSION ($ED)"
# kagent's OpenAI client insists on a key. agentgateway ignores it and injects
# the real provider credential, so this placeholder is all kagent ever holds.
K create secret generic kagent-llm -n kagent --from-literal=API_KEY=via-agentgateway \
  --dry-run=client -o yaml | K apply -f - >/dev/null
helm_up kagent-crds "$KAGENT_CRDS_CHART" "$KAGENT_VERSION" kagent
values_for "$D" values "$ED"
HELM_TIMEOUT=15m helm_up kagent "$KAGENT_CHART" "$KAGENT_VERSION" kagent ${VALS[@]+"${VALS[@]}"}
rollout kagent deploy/kagent-controller deploy/kagent-ui

step "Substrate worker pool"
wait_for "WorkerPool kagent-default" 30 5 K get workerpool kagent-default -n kagent
ok "kagent-default ready for SandboxAgents"

step "Edge SSO: https://kagent.${SV_DOMAIN}"
apply_tmpl "$D/edge-sso.yaml"
ok "kgateway OAuth2 -> S&V Keycloak; access token forwarded to kagent"
