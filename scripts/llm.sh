#!/usr/bin/env bash
# Point the lab's single LLM surface at a provider:
#   make llm                       # uses LLM_PROVIDER from .env (default ollama)
#   make llm LLM_PROVIDER=anthropic
#   make llm LLM_FALLBACK=anthropic  # fail over to a second provider (or
#                                    # LLM_FALLBACK=ollama LLM_FALLBACK_MODEL=...)
# Keys come from .env. Agents never see them: agentgateway injects the
# credential on the way out.
. "$(dirname "$0")/lib.sh"
D="$LAB_ROOT/platform/40-agentgateway/llm"
need_cluster
P="${LLM_PROVIDER:-ollama}"
F="${LLM_FALLBACK:-}"   # a second provider the gateway fails over to (optional)

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
    *) die "LLM provider must be ollama | anthropic | openai, not $1" ;;
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
  first=$(provider primary "$P")
  prepare "$F" "${LLM_FALLBACK_MODEL:-}"; FMODEL=$PREPARED_MODEL
  [ "$F/$FMODEL" != "$P/$MODEL" ] || die "the fallback is the primary ($P/$MODEL): set LLM_FALLBACK_MODEL"
  second=$(provider fallback "$F")
  # groups in priority order: the gateway uses the first group with a healthy
  # provider, and evicts one that fails (policy.yaml)
  render "$D/$P.yaml" | yq -o json | jq -c --argjson a "$first" --argjson b "$second" --arg f "$F" \
    'del(.spec.ai.provider, .spec.policies) | .metadata.labels["lab.solo.io/llm-fallback"] = $f
     | .spec.ai.groups = [{providers: [$a]}, {providers: [$b]}]' | K apply -f - >/dev/null
fi
apply_tmpl "$D/route.yaml" "$D/policy.yaml"
K annotate namespace agentgateway-system lab.solo.io/llm-provider="$P" lab.solo.io/llm-model="$MODEL" \
  lab.solo.io/llm-fallback="${F:+$F/$FMODEL}" --overwrite >/dev/null
ok "llm -> $P ($MODEL)${F:+, then $F ($FMODEL)}"
