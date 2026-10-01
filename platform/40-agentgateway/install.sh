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
CRDS=$AGW_CRDS_CHART CRDS_VERSION=$AGW_VERSION
if [ "$ED" = enterprise ]; then
  # Enterprise kgateway and agentgateway both ship Solo's ext-auth, rate-limit
  # and WAF CRDs, and a CRD can only belong to one helm release: leave out any
  # another release (enterprise kgateway, layer 30) already owns.
  pristine="$LAB_STATE/cache/enterprise-agentgateway-crds-$AGW_VERSION"
  if [ ! -d "$pristine" ]; then
    rm -rf "$pristine.tmp"
    H pull "$AGW_CRDS_CHART" --version "$AGW_VERSION" --untar --untardir "$pristine.tmp" >"$LAB_STATE/helm-agw-crds-pull.log" 2>&1 \
      || die "pulling $AGW_CRDS_CHART $AGW_VERSION failed (log: .lab/helm-agw-crds-pull.log)"
    mv "$pristine.tmp/enterprise-agentgateway-crds" "$pristine"; rm -rf "$pristine.tmp"
  fi
  CRDS="$LAB_STATE/enterprise-agentgateway-crds"; CRDS_VERSION=""
  rm -rf "$CRDS"; cp -R "$pristine" "$CRDS"
  for f in "$CRDS"/templates/*.yaml; do
    # templates wrap CRDs in {{ if }} blocks, and a file may hold several
    for name in $(sed 's/{{[^}]*}}//g' "$f" | yq -N 'select(.kind == "CustomResourceDefinition") | .metadata.name' 2>/dev/null); do
      owner=$(K get crd "$name" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)
      if [ -n "$owner" ] && [ "$owner" != "$AGW_RELEASE-crds" ]; then rm -f "$f"; ok "$(basename "$f" .yaml): kept by $owner"; break; fi
    done
  done
fi
helm_up "$AGW_RELEASE-crds" "$CRDS" "$CRDS_VERSION" agentgateway-system
values_for "$D" values "$ED"
helm_up "$AGW_RELEASE" "$AGW_CHART" "$AGW_VERSION" agentgateway-system ${VALS[@]+"${VALS[@]}"}

step "AI gateway (agentgateway-system/ai-gateway)"
apply_tmpl "$D/ai-gateway.yaml"
wait_for "ai-gateway Programmed" 60 3 \
  K wait -n agentgateway-system gateway/ai-gateway --for=condition=Programmed --timeout=2s
ok "ai-gateway programmed — in-cluster: http://ai-gateway.agentgateway-system"

"$LAB_ROOT/scripts/llm.sh"
