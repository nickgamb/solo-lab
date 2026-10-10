#!/usr/bin/env bash
# kagent + kmcp (bundled subchart), wired to the platform:
#   model   -> agentgateway (no provider key in kagent, ever)
#   runtime -> pods, or Agent Substrate for SandboxAgents
#   traces  -> otel-collector
#   oss:        kagent 0.10.3 + tools/kagent
#   enterprise: Solo Enterprise for kagent 0.5.10 + tools/kagent's Go ADK
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
ED=$KAGENT_EDITION

# tools/kagent builds the controller (OSS) and the Go ADK runtime (both editions)
lab_image "kagent-dev/kagent/controller:$KAGENT_LAB_TAG" tools/kagent/build.sh
step "kagent $KAGENT_VERSION ($ED)"
if [ "$ED" = enterprise ]; then
  # the enterprise controller pins the Go ADK by digest: point it at ours
  KAGENT_LAB_GOADK_DIGEST=$(curl -sfI "http://localhost:$LAB_REGISTRY_PORT/v2/kagent-dev/kagent/golang-adk/manifests/$KAGENT_LAB_TAG" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    | tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest"{print $2}')
  [ -n "$KAGENT_LAB_GOADK_DIGEST" ] || die "no golang-adk:$KAGENT_LAB_TAG in the lab registry (tools/kagent/build.sh)"
  export KAGENT_LAB_GOADK_DIGEST
  [ -n "$SOLO_KAGENT_LICENSE_KEY" ] || warn "no SOLO_KAGENT_LICENSE_KEY (or SOLO_LICENSE_KEY) in .env: kagent-enterprise runs, and logs that it's unlicensed"
  printf '%s' "$SOLO_KAGENT_LICENSE_KEY" | K create secret generic enterprise-kagent-license -n kagent \
    --from-file=enterprise-kagent-license-key=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
fi
# kagent's OpenAI client insists on a key. agentgateway ignores it and injects
# the real provider credential, so this placeholder is all kagent ever holds.
secret_apply kagent kagent-llm API_KEY=via-agentgateway
# The ops worker pool's identity (the chart's WorkerPool runs as it)
K create serviceaccount kagent-ops -n kagent --dry-run=client -o yaml | K apply -f - >/dev/null
helm_up kagent-crds "$KAGENT_CRDS_CHART" "$KAGENT_VERSION" kagent
values_for "$D" values "$ED"
HELM_TIMEOUT=15m helm_up kagent "$KAGENT_CHART" "$KAGENT_VERSION" kagent ${VALS[@]+"${VALS[@]}"}
# before the rollout: the enterprise controller restarts until it can read GatewayClasses
[ "$ED" = enterprise ] && apply_tmpl "$D/enterprise.yaml"
if [ "$ED" = enterprise ]; then rollout kagent deploy/kagent-controller; else rollout kagent deploy/kagent-controller deploy/kagent-ui; fi

# The chart's built-in agents (k8s, kgateway, istio, helm, promql), rendered
# from the same chart as SandboxAgents on the ops pool. Each forwards the
# signed-in user's token on its tool calls, for the gateway to verify
# (front-door.yaml), and the tools they name are the only ones the gateway
# lets through to kagent-tools.
OPS="k8s-agent kgateway-agent istio-agent helm-agent promql-agent"
on=$(printf ',%s.enabled=true' $OPS)
OPS_AGENTS=$(H template kagent "$KAGENT_CHART" --version "$KAGENT_VERSION" -n kagent ${VALS[@]+"${VALS[@]}"} --set "${on#,}" 2>"$LAB_STATE/helm-ops-agents.log" \
  | yq 'select(.kind == "Agent") | .kind = "SandboxAgent" | .spec.substrate.workerPoolRef.name = "kagent-ops"
    | .spec.declarative.deployment.env += [{"name": "KAGENT_PROPAGATE_TOKEN", "value": "true"}]
    | del(.metadata.labels."helm.sh/chart") | .metadata.labels."app.kubernetes.io/managed-by" = "solo-lab"') \
  || die "rendering the ops agents failed (.lab/helm-ops-agents.log)"
KAGENT_TOOLS_ALLOWED=$(echo "$OPS_AGENTS" | yq -o=json -I=0 '.' \
  | jq -sc '[.[].spec.declarative.tools[]? | select(.mcpServer.name == "kagent-tool-server") | .mcpServer.toolNames[]] | unique')
[ "$KAGENT_TOOLS_ALLOWED" != "[]" ] || die "the ops agents name no kagent-tools tools"
export KAGENT_TOOLS_ALLOWED

step "Ops tools behind the firm's gateway (ai-gateway/mcp/kagent-tools)"
# Like every MCP server: kagent-tools is its own release, and the ops agents'
# tool server (kagent-tool-server, the name the chart's agents use) is the
# gateway's path to it.
values_for "$D" tools-values "$ED"
helm_up kagent-tools "$KAGENT_TOOLS_CHART" "$KAGENT_TOOLS_VERSION" kagent ${VALS[@]+"${VALS[@]}"}
apply_tmpl "$D/front-door.yaml"
ok "kagent-tools: the ops agents' workers, for a platform admin, $(echo "$KAGENT_TOOLS_ALLOWED" | jq length) tools; the controller lists them"

step "Ops agents on Agent Substrate (kagent-ops)"
# A SandboxAgent can't share a name with an Agent (config Secret, session
# ids), so a helm-made Agent must be gone first.
for a in $OPS; do K wait "agent/$a" -n kagent --for=delete --timeout=120s >/dev/null 2>&1 || true; done
echo "$OPS_AGENTS" | K apply -f - >/dev/null
for a in $OPS; do wait_for "SandboxAgent $a Ready" 120 5 K wait "sandboxagent/$a" -n kagent --for=condition=Ready --timeout=2s; done
ok "$OPS"

if [ "$ED" = enterprise ]; then
  # the Solo UI (layer 90) is kagent's UI, with its own sign-in; earlier labs
  # had the chart's UI behind the edge's SSO, and its /api route
  K delete httproute kagent-ui kagent-api -n kagent --ignore-not-found >/dev/null
  K delete trafficpolicies.gateway.kgateway.dev kagent-sso kagent-api-sso -n kagent --ignore-not-found >/dev/null
  K delete authorizationpolicy kagent-controller-edge -n kagent --ignore-not-found >/dev/null
  ok "kagent's UI: the Solo UI at https://kagent.${SV_DOMAIN} (layer 90)"
else
  step "Edge SSO: https://kagent.${SV_DOMAIN}"
  apply_tmpl "$D/edge-sso.yaml"
  ok "kgateway OAuth2 -> S&V Keycloak; access token forwarded to kagent"
fi
