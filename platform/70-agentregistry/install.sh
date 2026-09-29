#!/usr/bin/env bash
# agentregistry: Sterling & Vance's catalog of agents, MCP servers and skills.
# OSS 0.4.0 has no authentication of its own, so it sits behind S&V's SSO at
# the kgateway edge (https://registry.sterling.lab), and it may only manage
# kagent resources in sv-agents.
#   agentregistry-enterprise (native OIDC) isn't wired here; docs/ENTERPRISE.md
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
step "agentregistry $AGENTREGISTRY_VERSION"
helm_up agentregistry "$AGENTREGISTRY_CHART" "$AGENTREGISTRY_VERSION" agentregistry -f "$D/values.yaml"
K create secret generic agentregistry-oidc -n agentregistry \
  --from-literal=client-secret="$(lab_secret SV_AGENTREGISTRY_CLIENT_SECRET)" --dry-run=client -o yaml | K apply -f - >/dev/null
apply_tmpl "$D/edge-sso.yaml"
ok "https://registry.${SV_DOMAIN} (S&V SSO at the edge)"
