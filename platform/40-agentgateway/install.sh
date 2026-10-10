#!/usr/bin/env bash
# agentgateway — the AI gateway: LLM egress and MCP federation.
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
CRD_VALUES=()
if [ "$ED" = enterprise ]; then
  # Enterprise kgateway and agentgateway both ship Solo's ext-auth, rate-limit,
  # WAF and enterprise.solo.io CRDs, and a CRD can only belong to one helm
  # release: the chart's own switches leave out any another release
  # (enterprise kgateway, layer 30) already owns.
  for t in installExtAuthCRDs=authconfigs.extauth.solo.io installRateLimitCRDs=ratelimitconfigs.ratelimit.solo.io \
           installWAFPolicyCRD=wafpolicies.waf.solo.io installEnterpriseGatewayCRD=enterpriselistenersets.enterprise.solo.io; do
    owner=$(K get crd "${t#*=}" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)
    if [ -n "$owner" ] && [ "$owner" != "$AGW_RELEASE-crds" ]; then CRD_VALUES+=(--set "${t%%=*}=false"); ok "${t#*=}: kept by $owner"; fi
  done
fi
helm_up "$AGW_RELEASE-crds" "$AGW_CRDS_CHART" "$AGW_VERSION" agentgateway-system ${CRD_VALUES[@]+"${CRD_VALUES[@]}"}
values_for "$D" values "$ED"
helm_up "$AGW_RELEASE" "$AGW_CHART" "$AGW_VERSION" agentgateway-system ${VALS[@]+"${VALS[@]}"}

step "AI gateway (agentgateway-system/ai-gateway)"
# Enterprise: the class's shared services (Solo's ext-auth service
# replicated), attached to the class
if [ "$ED" = enterprise ]; then
  apply_tmpl "$D/shared-extensions.yaml"
  K patch gatewayclass enterprise-agentgateway --type merge -p '{"spec":{"parametersRef":{"group":"enterpriseagentgateway.solo.io","kind":"EnterpriseAgentgatewayParameters","name":"shared-extensions","namespace":"agentgateway-system"}}}' >/dev/null
  ok "Solo's ext-auth service: 3 replicas across zones, never fewer than 2 (shared-extensions.yaml)"
fi
K apply -f "$D/llm/costs.yaml" >/dev/null
apply_tmpl "$D/ai-gateway.yaml"
# Enterprise: the STS that mints the tokens MCP servers accept (values-enterprise.yaml)
if [ "$ED" = enterprise ]; then
  apply_tmpl "$D/sts.yaml"   # its keys, for MCP servers
  ok "STS: $MCP_TOKEN_ISSUER (tokens for MCP servers, $MCP_TOKEN_LIFETIME)"
fi
K delete referencegrant gateways-to-sts -n agentgateway-system --ignore-not-found >/dev/null   # earlier labs
wait_for "ai-gateway Programmed" 60 3 \
  K wait -n agentgateway-system gateway/ai-gateway --for=condition=Programmed --timeout=2s
ok "ai-gateway programmed — in-cluster: http://ai-gateway.agentgateway-system"
# ai-gateway itself reaches the LLM providers. The services beside it (S&V's
# ID-token exchange and MCP guardrail, demos/bob) get the cluster and nothing
# else: the guardrail sees every tool result unmasked.
deny_internet_pods agentgateway-system idtoken-exchange mcp-guard
ok "no direct internet from idtoken-exchange, mcp-guard (agentgateway-system)"

"$LAB_ROOT/scripts/llm.sh"
