#!/usr/bin/env bash
# Point the lab's single LLM surface at a provider:
#   make llm                       # uses LLM_PROVIDER from .env (default ollama)
#   make llm LLM_PROVIDER=anthropic   # ollama | anthropic | openai | bedrock
#   make llm LLM_FALLBACK=anthropic  # fail over to a second provider (or
#                                    # LLM_FALLBACK=ollama LLM_FALLBACK_MODEL=...)
#   make llm LLM_PROVIDER=bedrock LLM_FALLBACK=openai,ollama
#                                    # fallbacks in order; LLM_FALLBACK_MODEL
#                                    # takes one model each, comma-separated
# Keys come from .env. Agents never see them: agentgateway injects the
# credential on the way out.
. "$(dirname "$0")/lib.sh"
D="$LAB_ROOT/platform/40-agentgateway/llm"
need_cluster
P="${LLM_PROVIDER:-ollama}"
F="${LLM_FALLBACK:-}"   # providers the gateway fails over to, in order (optional, comma-separated)

# prepare <provider> [model]: its credential in the cluster and the variables
# its backend file renders with; sets PREPARED_MODEL (not a subshell: the
# variables are needed here)
prepare() {
  case "$1" in
    ollama)
      if [ -z "${OLLAMA_URL:-}" ]; then
        if docker_vm; then OLLAMA_URL=http://host.docker.internal:11434
        else
          gw=$(docker network inspect kind --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -m1 '\.')
          OLLAMA_URL="http://${gw:?no IPv4 gateway on the kind network}:11434"
        fi
      fi
      hp="${OLLAMA_URL#*://}"; hp=${hp%%/*}; export OLLAMA_HOST="${hp%%:*}" OLLAMA_PORT="${hp##*:}"
      [ "$OLLAMA_PORT" != "$hp" ] || OLLAMA_PORT=11434
      export OLLAMA_MODEL="${2:-${OLLAMA_MODEL:-qwen3.8:27b}}"
      curl -sf -m 3 "$(ollama_host_url)/api/tags" | jq -e --arg m "$OLLAMA_MODEL" '.models[] | select(.name==$m)' >/dev/null \
        || warn "model $OLLAMA_MODEL not found at $(ollama_host_url) (ollama pull $OLLAMA_MODEL)"
      PREPARED_MODEL=$OLLAMA_MODEL ;;
    anthropic)
      [ -n "${ANTHROPIC_API_KEY:-}" ] || die "ANTHROPIC_API_KEY is empty (set it in .env)"
      export ANTHROPIC_MODEL="${2:-${ANTHROPIC_MODEL:-claude-sonnet-5}}"
      printf '%s' "$ANTHROPIC_API_KEY" | K create secret generic llm-anthropic -n agentgateway-system \
        --from-file=Authorization=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
      PREPARED_MODEL=$ANTHROPIC_MODEL ;;
    openai)
      [ -n "${OPENAI_API_KEY:-}" ] || die "OPENAI_API_KEY is empty (set it in .env)"
      export OPENAI_MODEL="${2:-${OPENAI_MODEL:-gpt-5-mini}}"
      printf '%s' "$OPENAI_API_KEY" | K create secret generic llm-openai -n agentgateway-system \
        --from-file=Authorization=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
      PREPARED_MODEL=$OPENAI_MODEL ;;
    bedrock)
      export BEDROCK_MODEL="${2:-${BEDROCK_MODEL:-us.anthropic.claude-sonnet-4-6}}"
      export BEDROCK_REGION="${BEDROCK_REGION:-us-east-1}"
      # a Bedrock API key, or AWS credentials (the gateway signs with SigV4)
      if [ -n "${BEDROCK_API_KEY:-}" ]; then
        printf '%s' "$BEDROCK_API_KEY" | K create secret generic llm-bedrock -n agentgateway-system \
          --from-file=Authorization=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
        export BEDROCK_AUTH='{secretRef: {name: llm-bedrock}}'
      elif [ -n "${BEDROCK_AWS_ACCESS_KEY_ID:-}" ] && [ -n "${BEDROCK_AWS_SECRET_ACCESS_KEY:-}" ]; then
        K create secret generic llm-bedrock -n agentgateway-system \
          --from-literal=accessKey="$BEDROCK_AWS_ACCESS_KEY_ID" --from-literal=secretKey="$BEDROCK_AWS_SECRET_ACCESS_KEY" \
          ${BEDROCK_AWS_SESSION_TOKEN:+--from-literal=sessionToken="$BEDROCK_AWS_SESSION_TOKEN"} \
          --dry-run=client -o yaml | K apply -f - >/dev/null
        export BEDROCK_AUTH='{aws: {secretRef: {name: llm-bedrock}}}'
      else die "bedrock needs BEDROCK_API_KEY, or BEDROCK_AWS_ACCESS_KEY_ID and BEDROCK_AWS_SECRET_ACCESS_KEY (set them in .env)"
      fi
      PREPARED_MODEL=$BEDROCK_MODEL ;;
    *) die "LLM provider must be ollama | anthropic | openai | bedrock, not $1" ;;
  esac
}
# provider <name> <provider>: its backend file as one entry of a provider group
provider() {
  render "$D/$2.yaml" | yq -o json | jq -c --arg n "$1" \
    '.spec.ai.provider + {name: $n} + (if .spec.policies then {policies: .spec.policies} else {} end)'
}

step "LLM provider: $P${F:+, failing over to $F}"
prepare "$P"; MODEL=$PREPARED_MODEL FMODEL=
if [ -z "$F" ]; then
  render "$D/$P.yaml" | K apply -f - >/dev/null
else
  IFS=, read -r -a FS <<< "$F"; IFS=, read -r -a FMS <<< "${LLM_FALLBACK_MODEL:-}"
  groups="[{\"providers\": [$(provider primary "$P")]}]"; used=" $P/$MODEL "; FMODEL=
  for i in "${!FS[@]}"; do
    prepare "${FS[$i]}" "${FMS[$i]:-}"
    case "$used" in *" ${FS[$i]}/$PREPARED_MODEL "*) die "${FS[$i]}/$PREPARED_MODEL is already in the chain: set LLM_FALLBACK_MODEL" ;; esac
    used="$used${FS[$i]}/$PREPARED_MODEL "; FMODEL="${FMODEL:+$FMODEL, }${FS[$i]}/$PREPARED_MODEL"
    groups=$(jq -c --argjson p "$(provider "fallback$((i + 1))" "${FS[$i]}")" '. + [{providers: [$p]}]' <<< "$groups")
  done
  # groups in priority order: the gateway uses the first group with a healthy
  # provider, and evicts one that fails (policy.yaml)
  render "$D/$P.yaml" | yq -o json | jq -c --argjson g "$groups" --arg f "${F//,/.}" \
    'del(.spec.ai.provider, .spec.policies) | .metadata.labels["lab.solo.io/llm-fallback"] = $f
     | .spec.ai.groups = $g' | K apply -f - >/dev/null
fi
# The security tools' model (llm/security.yaml): the same provider, a model
# that answers in a JSON schema (LLM_SECURITY_MODEL, or the provider's below)
case "$P" in
  bedrock) SMODEL=us.anthropic.claude-haiku-4-5-20251001-v1:0 ;;
  anthropic) SMODEL=claude-haiku-4-5 ;;
  *) SMODEL=$MODEL ;;
esac
SMODEL=${LLM_SECURITY_MODEL:-$SMODEL}
prepare "$P" "$SMODEL"
render "$D/$P.yaml" | yq '.metadata.name = "llm-security"' | K apply -f - >/dev/null
apply_tmpl "$D/security.yaml"
# API keys for callers outside the mesh (llm/external.yaml): only their
# SHA-256 is in the cluster; the key itself is in .lab/secrets.env
# ("capped" has next to no budget on Solo Enterprise, to show a block)
hash() { printf 'sha256:%s' "$(printf '%s' "$1" | openssl dgst -sha256 -r | cut -d' ' -f1)"; }
jq -nc --arg d "$(hash "$(lab_secret LLM_API_KEY)")" --arg c "$(hash "$(lab_secret LLM_API_KEY_CAPPED)")" \
  '{apiVersion: "v1", kind: "ConfigMap", metadata: {name: "llm-api-keys", namespace: "agentgateway-system",
    labels: {"lab.solo.io/llm-api-keys": "true"}},
    data: {developer: ({keyHash: $d, metadata: {id: "developer", user_id: "developer"}} | tojson),
           capped: ({keyHash: $c, metadata: {id: "capped", user_id: "capped"}} | tojson)}}' | K apply -f - >/dev/null
apply_tmpl "$D/policy.yaml"
if [ "$AGW_EDITION" = enterprise ]; then
  # Solo Enterprise: per-agent token limits and budgets, on the routes'
  # existing policies (two policies on one route would override each other)
  apply_tmpl "$D/enterprise.yaml" || warn "token limits and budgets (llm/enterprise.yaml) not applied"
  render "$D/route.yaml" | yq '(select(.metadata.name == "llm-callers") | .spec.traffic) +=
    {"entRateLimit": {"global": {"rateLimitConfigRefs": [{"name": "llm-tokens-per-agent"}]}}, "entBudgetEnforcement": {}}' | K apply -f - >/dev/null \
    || { warn "per-agent token limits not attached; the route without them"; apply_tmpl "$D/route.yaml"; }
  render "$D/external.yaml" | yq '(select(.metadata.name == "llm-external" and .kind != "HTTPRoute") | .spec.traffic) +=
    {"entBudgetEnforcement": {}}' | K apply -f - >/dev/null \
    || { warn "budgets not attached to the external route"; apply_tmpl "$D/external.yaml"; }
else
  apply_tmpl "$D/route.yaml" "$D/external.yaml"
fi
K annotate namespace agentgateway-system lab.solo.io/llm-provider="$P" lab.solo.io/llm-model="$MODEL" \
  lab.solo.io/llm-fallback="$FMODEL" lab.solo.io/llm-security-model="$SMODEL" --overwrite >/dev/null
ok "llm -> $P ($MODEL)${F:+, then $FMODEL}; security tools -> $SMODEL"
