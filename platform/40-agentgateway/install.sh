#!/usr/bin/env bash
# agentgateway — the AI gateway: LLM egress, MCP federation, A2A.
# The agents' worker pools reach models and cross-company tools only through
# here, so provider credentials, per-caller policy and access logs live in one
# place. (Rate limits and prompt guards would go here too; the lab sets none.)
#   oss:        agentgateway            (GatewayClass agentgateway)
#   enterprise: Solo Enterprise for agentgateway (GatewayClass enterprise-agentgateway)
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
ED=$AGW_EDITION

step "agentgateway $AGW_VERSION ($ED)"
helm_up "$AGW_RELEASE-crds" "$AGW_CRDS_CHART" "$AGW_VERSION" agentgateway-system
values_for "$D" values "$ED"
helm_up "$AGW_RELEASE" "$AGW_CHART" "$AGW_VERSION" agentgateway-system ${VALS[@]+"${VALS[@]}"}

step "AI gateway (agentgateway-system/ai-gateway)"
apply_tmpl "$D/ai-gateway.yaml"
wait_for "ai-gateway Programmed" 60 3 \
  K wait -n agentgateway-system gateway/ai-gateway --for=condition=Programmed --timeout=2s
ok "ai-gateway programmed — in-cluster: http://ai-gateway.agentgateway-system"

"$LAB_ROOT/scripts/llm.sh"
