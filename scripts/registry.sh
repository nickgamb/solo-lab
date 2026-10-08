#!/usr/bin/env bash
# agentregistry (AGENTREGISTRY_EDITION=enterprise): its catalog, who may
# browse it, and its kagent runtime.
#
# The catalog is what the lab runs: every agent kagent runs (its harness), with its instructions
# as a Prompt, its model, and the MCP servers it uses; and every MCP server
# kagent knows.
# Read from the cluster, so it's whatever the lab has now; each entry is
# labelled with its party (the namespace's lab.solo.io/party) and its
# Kubernetes namespace, in the registry's default namespace (what its UI
# lists first). Published as the catalog's service account (S&V client
# agentregistry-catalog, a platform admin there). Idempotent: apply
# replaces what's there. The groups in AGENTREGISTRY_READERS may browse it.
# With Solo Enterprise for kagent too, the registry connects to the kagent
# controller as a runtime (as its own client, agentregistry, a kagent
# Writer): it discovers the agents running there and can deploy from the
# catalog.
. "$(dirname "$0")/lib.sh"
need_cluster
step "agentregistry: the catalog, its readers and the kagent runtime"
if [ "$AGENTREGISTRY_EDITION" != enterprise ]; then
  ok "skipped: the catalog is published on agentregistry Enterprise (AGENTREGISTRY_EDITION=enterprise)"
  exit 0
fi

# each namespace's party, for the catalog's <org>/<name>
parties=$(K get ns -o json | jq -c '[.items[] | {key: .metadata.name, value: (.metadata.labels["lab.solo.io/party"] // .metadata.name)}] | from_entries')
mcps=$(K get remotemcpservers.kagent.dev -A -o json 2>/dev/null || echo '{"items":[]}')
agents=$(jq -s '{items: (map(.items) | add)}' \
  <(K get sandboxagents.kagent.dev -A -o json 2>/dev/null || echo '{"items":[]}') \
  <(K get agents.kagent.dev -A -o json 2>/dev/null || echo '{"items":[]}'))
models=$(K get modelconfigs.kagent.dev -A -o json 2>/dev/null || echo '{"items":[]}')

docs=$(jq -n --argjson p "$parties" --argjson m "$mcps" --argjson a "$agents" --argjson mc "$models" '
  def meta($ns; $name): {name: $name, labels: {"lab.solo.io/party": ($p[$ns] // $ns), "lab.solo.io/namespace": $ns}};
  def ar: "ar.dev/v1alpha1";
  ([$m.items[] | {apiVersion: ar, kind: "MCPServer",
      metadata: meta(.metadata.namespace; .metadata.name),
      spec: ({description: (.spec.description // .metadata.name),
        remote: {type: (if .spec.protocol == "SSE" then "sse" else "streamable-http" end), url: .spec.url}})}])
  + ([$a.items[] | select(.spec.declarative != null) | . as $ag | .spec.declarative as $d
      | ($mc.items[] | select(.metadata.namespace == $ag.metadata.namespace and .metadata.name == $d.modelConfig)) as $model
      | (if ($d.systemMessage // "") != "" then [{apiVersion: ar, kind: "Prompt",
          metadata: meta($ag.metadata.namespace; "\($ag.metadata.name)-instructions"),
          spec: {description: "\($ag.metadata.name)'"'"'s instructions", content: $d.systemMessage}}] else [] end)
      + [{apiVersion: ar, kind: "Agent",
          metadata: (meta($ag.metadata.namespace; $ag.metadata.name)
            | .labels += {"lab.solo.io/model-provider": ($model.spec.provider // "" | ascii_downcase), "lab.solo.io/model": ($model.spec.model // "")}),
          spec: ({description: ($ag.spec.description // $ag.metadata.name), source: {protocol: "A2A"},
              compatibleHarnesses: [{type: "kagent"}],
              mcpServers: [$d.tools[]? | select(.type == "McpServer" and .mcpServer.kind == "RemoteMCPServer")
                | {kind: "MCPServer", name: .mcpServer.name}]}
            + (if ($d.systemMessage // "") != "" then {instructions: {kind: "Prompt", name: "\($ag.metadata.name)-instructions"}} else {} end))}]
    ] | add // [])')
# who may browse it: the registry admits only its superusers (platform-admins)
# until an AccessPolicy grants a role more
docs=$(jq --arg readers "$AGENTREGISTRY_READERS" '. + [{apiVersion: "ar.dev/v1alpha1", kind: "AccessPolicy",
  metadata: {name: "catalog-readers"},
  spec: {description: "Browse the catalog (agents, MCP servers, prompts, skills), the runtimes and what runs there",
    principals: [$readers | split(" ")[] | select(. != "") | {kind: "Role", name: .}],
    rules: [{actions: ["registry:read"], resources: [{kind: "agent", name: "*"}, {kind: "server", name: "*"}, {kind: "prompt", name: "*"}, {kind: "skill", name: "*"},
      {kind: "runtime", name: "*"}]}]}}]' <<<"$docs")
# the kagent runtime: its client secret as a registry Secret, then the
# Runtime that uses it to call the kagent controller
runtime=none
if [ "$KAGENT_EDITION" = enterprise ]; then
  runtime=kagent
  docs=$(jq --arg secret "$(lab_secret SV_AGENTREGISTRY_CLIENT_SECRET)" --arg issuer "https://idp.$SV_DOMAIN/realms/sterling-vance" '. + [
    {apiVersion: "ar.dev/v1alpha1", kind: "Secret", metadata: {name: "kagent-oidc"},
     spec: {type: "Opaque", stringData: {clientSecret: $secret}}},
    {apiVersion: "ar.dev/v1alpha1", kind: "Runtime", metadata: {name: "kagent"},
     spec: {type: "Kagent",
       telemetryEndpoint: "http://agentregistry-enterprise-telemetry-collector.agentregistry.svc.cluster.local:4318",
       config: {kagentUrl: "http://kagent-controller.kagent:8083", namespace: "sv-agents",
         auth: {oidc: {issuer: $issuer, clientId: "agentregistry", clientSecretRef: {name: "kagent-oidc", key: "clientSecret"}}}}}}]' <<<"$docs")
fi
n_mcp=$(jq '[.[] | select(.kind == "MCPServer")] | length' <<<"$docs")
n_agent=$(jq '[.[] | select(.kind == "Agent")] | length' <<<"$docs")
n_prompt=$(jq '[.[] | select(.kind == "Prompt")] | length' <<<"$docs")
[ "$((n_mcp + n_agent))" -gt 0 ] || { warn "no agents or MCP servers in the cluster yet: nothing to publish"; exit 0; }

# the catalog's service account: client credentials, the secret on stdin
token=$(printf 'grant_type=client_credentials&client_id=agentregistry-catalog&client_secret=%s' "$(lab_secret SV_AGENTREGISTRY_CATALOG_SECRET)" \
  | curl -sf --cacert "$LAB_CA_DIR/ca.crt" -d @- "https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/token" | jq -r '.access_token // empty') \
  || die "no token for agentregistry-catalog from S&V's Keycloak (make layer-45 adds the client)"
[ -n "$token" ] || die "no token for agentregistry-catalog from S&V's Keycloak (make layer-45 adds the client)"

yaml=$(jq -c '.[]' <<<"$docs" | while read -r d; do echo '---'; yq -P <<<"$d"; done)
# the token as a header from a file descriptor, never an argument
out=$(curl -s --cacert "$LAB_CA_DIR/ca.crt" -H @<(printf 'Authorization: Bearer %s\n' "$token") -H 'Content-Type: application/yaml' \
  -X POST --data-binary @- -w '\n%{http_code}' "https://registry.$SV_DOMAIN/v0/apply" <<<"$yaml" 2>&1) || true
code=$(tail -1 <<<"$out")
[ "$code" = 200 ] || die "publishing to https://registry.$SV_DOMAIN failed ($code): $(sed '$d' <<<"$out" | head -c 600)"
# a 200 carries each document's own result
failed=$(sed '$d' <<<"$out" | jq -r '[.results[]? | select(.status == "failed") | "\(.kind) \(.namespace)/\(.name): \(.error)"] | .[:5][]')
[ -z "$failed" ] || die "agentregistry refused some of the catalog:
$failed"
ok "published $n_agent agents, $n_prompt prompts and $n_mcp MCP servers, readable by: $AGENTREGISTRY_READERS; runtime: $runtime (https://registry.$SV_DOMAIN)"
if [ "$runtime" = kagent ]; then
  # the registry connects to the kagent controller on its own; its discovery
  # lists what runs there as unmanaged instances a moment later
  synced=$(curl -s --cacert "$LAB_CA_DIR/ca.crt" -H @<(printf 'Authorization: Bearer %s\n' "$token") "https://registry.$SV_DOMAIN/v0/runtimes/kagent" \
    | jq -r '[.status.conditions[]? | select(.type == "Synced") | .status][0] // "unknown"')
  [ "$synced" = True ] && ok "runtime kagent: connected to the kagent controller (sv-agents)" \
    || warn "runtime kagent: not synced yet ($synced): GET /v0/runtimes/kagent says why"
fi
