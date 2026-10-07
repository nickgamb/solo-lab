#!/usr/bin/env bash
# Point the lab's single LLM surface at a provider:
#   make llm                       # uses LLM_PROVIDER from .env (default ollama)
#   make llm LLM_PROVIDER=anthropic
# Keys come from .env. Agents never see them: agentgateway injects the
# credential on the way out.
. "$(dirname "$0")/lib.sh"
D="$LAB_ROOT/platform/40-agentgateway/llm"
need_cluster
P="${LLM_PROVIDER:-ollama}"

step "LLM provider: $P"
case "$P" in
  ollama)
    # Ollama on this machine, as the kind nodes reach it: Docker Desktop,
    # OrbStack and Colima name the host host.docker.internal; Docker Engine
    # doesn't, and the host is the kind network's gateway (Ollama must listen
    # beyond loopback there: OLLAMA_HOST=0.0.0.0 in its service).
    if [ -z "${OLLAMA_URL:-}" ]; then
      if docker_vm; then OLLAMA_URL=http://host.docker.internal:11434
      else
        gw=$(docker network inspect kind --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -m1 '\.')
        OLLAMA_URL="http://${gw:?no IPv4 gateway on the kind network}:11434"
      fi
    fi
    hp="${OLLAMA_URL#*://}"; hp=${hp%%/*}; export OLLAMA_HOST="${hp%%:*}" OLLAMA_PORT="${hp##*:}"
    [ "$OLLAMA_PORT" != "$hp" ] || OLLAMA_PORT=11434
    export OLLAMA_MODEL="${OLLAMA_MODEL:-qwen3.8:27b}"
    curl -sf -m 3 "$(ollama_host_url)/api/tags" | jq -e --arg m "$OLLAMA_MODEL" '.models[] | select(.name==$m)' >/dev/null \
      || warn "model $OLLAMA_MODEL not found at $(ollama_host_url) (ollama pull $OLLAMA_MODEL)"
    MODEL=$OLLAMA_MODEL ;;
  anthropic)
    [ -n "${ANTHROPIC_API_KEY:-}" ] || die "ANTHROPIC_API_KEY is empty (set it in .env)"
    export ANTHROPIC_MODEL="${ANTHROPIC_MODEL:-claude-sonnet-5}"
    # the key reaches kubectl on stdin, not its command line
    printf '%s' "$ANTHROPIC_API_KEY" | K create secret generic llm-anthropic -n agentgateway-system \
      --from-file=Authorization=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
    MODEL=$ANTHROPIC_MODEL ;;
  openai)
    [ -n "${OPENAI_API_KEY:-}" ] || die "OPENAI_API_KEY is empty (set it in .env)"
    export OPENAI_MODEL="${OPENAI_MODEL:-gpt-5-mini}"
    # the key reaches kubectl on stdin, not its command line
    printf '%s' "$OPENAI_API_KEY" | K create secret generic llm-openai -n agentgateway-system \
      --from-file=Authorization=/dev/stdin --dry-run=client -o yaml | K apply -f - >/dev/null
    MODEL=$OPENAI_MODEL ;;
  *) die "LLM_PROVIDER must be ollama | anthropic | openai" ;;
esac

render "$D/$P.yaml" | K apply -f - >/dev/null
apply_tmpl "$D/route.yaml"
K annotate namespace agentgateway-system lab.solo.io/llm-provider="$P" lab.solo.io/llm-model="$MODEL" --overwrite >/dev/null
ok "llm -> $P ($MODEL)"
