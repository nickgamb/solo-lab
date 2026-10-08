#!/usr/bin/env bash
# agentregistry's catalog, from what the lab runs (AGENTREGISTRY_EDITION=
# enterprise): every agent kagent runs (its harness), with its instructions
# as a Prompt, its model, and the MCP servers it uses; and every MCP server
# kagent knows.
# Read from the cluster, so it's whatever the lab has now; each entry is
# labelled with its party (the namespace's lab.solo.io/party) and its
# Kubernetes namespace, in the registry's default namespace (what its UI
# lists first). Published as the catalog's service account (S&V client
# agentregistry-catalog, a platform admin there). Idempotent: apply
# replaces what's there.
. "$(dirname "$0")/lib.sh"
need_cluster
step "agentregistry catalog: the lab's agents and MCP servers"
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
          metadata: meta($ag.metadata.namespace; $ag.metadata.name),
          spec: ({description: ($ag.spec.description // $ag.metadata.name), source: {protocol: "A2A"},
              compatibleHarnesses: [{type: "kagent"}],
              modelProvider: ($model.spec.provider // ""), modelName: ($model.spec.model // ""),
              mcpServers: [$d.tools[]? | select(.type == "McpServer" and .mcpServer.kind == "RemoteMCPServer")
                | {kind: "MCPServer", name: .mcpServer.name}]}
            + (if ($d.systemMessage // "") != "" then {instructions: {kind: "Prompt", name: "\($ag.metadata.name)-instructions"}} else {} end))}]
    ] | add // [])')
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
ok "published $n_agent agents, $n_prompt prompts and $n_mcp MCP servers (https://registry.$SV_DOMAIN)"
