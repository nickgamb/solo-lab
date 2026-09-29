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
    # Ollama on this machine, as the kind nodes reach it: Docker Desktop
    # names the host host.docker.internal; Docker Engine doesn't, and the host
    # is the kind network's gateway (Ollama must listen beyond loopback there:
    # OLLAMA_HOST=0.0.0.0 in its service).
    if [ -z "${OLLAMA_URL:-}" ]; then
      if docker_desktop; then OLLAMA_URL=http://host.docker.internal:11434
      else
        gw=$(docker network inspect kind --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -m1 '\.')
        OLLAMA_URL="http://${gw:?no IPv4 gateway on the kind network}:11434"
      fi
    fi
    hp="${OLLAMA_URL#*://}"; export OLLAMA_HOST="${hp%%:*}" OLLAMA_PORT="${hp##*:}"
    export OLLAMA_MODEL="${OLLAMA_MODEL:-qwen3.8:27b}"
    curl -s -m 3 "http://localhost:$OLLAMA_PORT/api/tags" | jq -e --arg m "$OLLAMA_MODEL" '.models[] | select(.name==$m)' >/dev/null \
      || warn "model $OLLAMA_MODEL not found in local ollama (ollama pull $OLLAMA_MODEL)"
    MODEL=$OLLAMA_MODEL ;;
  anthropic)
    [ -n "${ANTHROPIC_API_KEY:-}" ] || die "ANTHROPIC_API_KEY is empty (set it in .env)"
    export ANTHROPIC_MODEL="${ANTHROPIC_MODEL:-claude-sonnet-5}"
    K create secret generic llm-anthropic -n agentgateway-system \
      --from-literal=Authorization="$ANTHROPIC_API_KEY" --dry-run=client -o yaml | K apply -f - >/dev/null
    MODEL=$ANTHROPIC_MODEL ;;
  openai)
    [ -n "${OPENAI_API_KEY:-}" ] || die "OPENAI_API_KEY is empty (set it in .env)"
    export OPENAI_MODEL="${OPENAI_MODEL:-gpt-5-mini}"
    K create secret generic llm-openai -n agentgateway-system \
      --from-literal=Authorization="$OPENAI_API_KEY" --dry-run=client -o yaml | K apply -f - >/dev/null
    MODEL=$OPENAI_MODEL ;;
  *) die "LLM_PROVIDER must be ollama | anthropic | openai" ;;
esac

render "$D/$P.yaml" | K apply -f - >/dev/null
apply_tmpl "$D/route.yaml"
K annotate namespace agentgateway-system lab.solo.io/llm-provider="$P" lab.solo.io/llm-model="$MODEL" --overwrite >/dev/null
ok "llm -> $P ($MODEL)"
