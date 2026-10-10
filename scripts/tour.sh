#!/usr/bin/env bash
# A paced, end-to-end drive of the lab to watch in the Observatory
# (docs/cards/observatory.html). Each scene sends real traffic along a real
# path, then says where to look. Bob's questions go to his actual agent
# (kagent -> Agent Substrate -> ai-gateway -> tools), so they need a model
# (make llm). Refusals come from probe pods with their own mesh identities.
#
#   make tour                 wait for Enter between scenes
#   make tour TOUR_AUTO=1     no prompts: TOUR_PAUSE seconds between scenes
#   make tour TOUR_FROM=4     start at a scene
#
# It leaves state behind (Alice's grants, agent sessions); make reset clears it.
. "$(dirname "$0")/lib.sh"
need_cluster; need_password_grant
PAUSE=${TOUR_PAUSE:-12} FROM=${TOUR_FROM:-1}
AUTO=${TOUR_AUTO:-}; [ -t 0 ] || AUTO=1
CA=(--cacert "$LAB_CA_DIR/ca.crt")
_B=$'\033[1m' _D=$'\033[2m' _N=$'\033[0m'

scene() {  # scene <n> <title>: returns false when skipped by TOUR_FROM
  [ "$1" -ge "$FROM" ] || return 1
  printf '\n%s━━ %s. %s %s\n' "$_B" "$1" "$2" "$_N"
}
look() { printf '   %s👁  %s%s\n' "$_D" "$*" "$_N"; }
say()  { printf '   %s\n' "$*"; }
next() {
  if [ -n "$AUTO" ]; then sleep "$PAUSE"
  else printf '\n   %s[Enter] next scene%s ' "$_D" "$_N"; read -r _; fi
}

step "Signing in (Bob at S&V, Alice at her own IdP)"
. "$LAB_ROOT/scripts/idp.sh"
# Bob signs in through S&V's own IdP, which a script can drive; for that step
# only: the chain goes back as it was (scene 5 cuts the IdP ahead of it)
prefer_tier keycloak
BOB=$(sso_token bob bob-demo | jq -r '.access_token // empty') || BOB=""
restore_tiers wait
[ -n "$BOB" ] || die "could not get Bob's token"
probe_pod "$KAGENT_UI_NS" "$KAGENT_UI_SA"; probe_pod sv-agents bob-assistant; probe_pod sv-agents; probe_pod observability
AGENT=sv-agents/probe-bob-assistant   # Bob's agent's workload identity
on_exit 'kill $(jobs -p) 2>/dev/null'
ok "tokens for bob; probe pods in kagent, sv-agents, observability"

CTX=$(new_uuid)   # one conversation, like one chat in the UI
ask() {  # ask <question>: Bob asks his agent through kagent, prints its reply
  local body reply
  say "Bob: ${_B}$1${_N}"
  body=$(jq -nc --arg q "$1" --arg c "$CTX" '{jsonrpc:"2.0",id:"1",method:"message/send",params:{message:{role:"user",kind:"message",messageId:(now|tostring),contextId:$c,parts:[{kind:"text",text:$q}]}}}')
  reply=$(a2a_send "$BOB" "$body" | jq -r '[.result.history[]? | select(.role=="agent") | .parts[]? | .text // empty] | last // "no reply"' 2>/dev/null) \
    || reply="no reply (the call to the agent failed)"
  say "Agent: $(echo "$reply" | tr '\n' ' ' | cut -c1-240)"
}
probe() {  # probe <ns>[/<pod>] <label> <url> <args...>: one call from a probe pod's identity
  local ns=${1%%/*} pod=probe label=$2; [[ $1 == */* ]] && pod=${1#*/}; shift 2
  local out; out=$(probe_exec "$ns/$pod" "$@" 2>&1 | tail -1 || true)
  local code what; code=$(echo "$out" | jq -r '.http // empty' 2>/dev/null)
  case "$code" in
    0|"") what="refused: the mesh closed the connection" ;;
    401) what="refused: 401, no token this server accepts" ;;
    403) what="refused: 403, policy" ;;
    400) what="refused: tool not offered to this caller" ;;
    2*) echo "$out" | grep -q '"isError": true' && what="refused by the tool" || what="allowed ($code)" ;;
    *) what="HTTP $code" ;;
  esac
  printf '   %-52s %s\n' "$label" "$what"
}

if ! why=$(llm_ready); then warn "Bob's agent has no model to answer with ($why): its replies will be empty (make llm)"; fi

cat <<EOF

  Open ${_B}https://observatory.$OPS_DOMAIN${_N} (ops / ops-demo) on the other screen.
  Start on ${_B}Topology${_N}, view ${_B}All${_N}. Every scene below is real traffic on a real path.
EOF
next

if scene 1 "Bob's agent, on Bob's tools (delegation)"; then
  look "Topology: kagent-controller → the bob-assistant tray: a worker bay lights as the actor wakes"
  look "then the agent → ai-gateway, for the model and for bob-workspace: one front door for both"
  ask "Who are you acting for, and what does the workspace see? Use whoami."
  ask "List my clients and their stage."
  look "Traffic: filter mcp and turn on Carries a token. Expand tools/call whoami: Bob's claims,"
  look "verified by ai-gateway (audience ai-gateway, groups advisors). The tool got a token of its own, minted for the call."
  next
fi

if scene 2 "Cross App Access to Ledgerline (ID-JAG)"; then
  look "Topology: ai-gateway → ledgerline-research, badged kgateway edge and Ledgerline's waypoint"
  ask "What is Ledgerline's view on technology, and which Ledgerline account am I using?"
  look "Traffic: the call to mcp.ledgerline.lab. The agent held only Bob's S&V access token:"
  look "ai-gateway got Bob's ID token from S&V's broker, traded it there for an ID-JAG, and Ledgerline issued its own token."
  next
fi

if scene 3 "What isn't allowed (probe pods, no model)"; then
  look "Traffic: turn on only denied. Each row is a refusal; the line below names the layer."
  GW=http://ai-gateway.agentgateway-system/mcp/bob-workspace XAA=http://ai-gateway.agentgateway-system/xaa/ledgerline/mcp
  POD_IP=$(K get pod -n sv-mcp -l app.kubernetes.io/name=bob-workspace -o jsonpath='{.items[0].status.podIP}')
  probe $AGENT        "export_book as an advisor (compliance only)"       "$GW" call export_book '{}' --token "$BOB"
  probe $AGENT        "an agent workload with no user token"              "$GW" call whoami '{}'
  probe observability "Bob's token from the wrong workload"               "$GW" call whoami '{}' --token "$BOB"
  probe sv-agents     "Bob's token from a non-agent pod beside the agents" "$GW" call whoami '{}' --token "$BOB"
  probe $AGENT        "skip the front door: dial the tool's pod directly" "http://$POD_IP:3000/mcp" call whoami '{}' --token "$BOB"
  probe observability "Ledgerline via XAA from the wrong workload"        "$XAA" call account_info '{}' --token "$BOB"
  probe $AGENT        "straight to Ledgerline with Bob's S&V token"       "https://mcp.$LEDGERLINE_DOMAIN/mcp" call account_info '{}' --token "$BOB"
  look "Topology, view Cross-party: the only wires left are the calls that cross a company boundary."
  next
fi

if scene 4 "Bob to Alice (UMA for agents)"; then
  if ! K get deploy uma-as -n alice >/dev/null 2>&1; then warn "story 2 isn't installed: skipping"; else
    look "Topology: bob-assistant → u4a-adapter → Meridian's gateway → uma-pep → Alice's authorization server"
    look "Alice's lane lights up. On a first contact the call waits for her; the tour approves as her."
    ALICE=$(user_token alice-identity alice alice-portal "" alice alice-demo) || ALICE=""
    [ -n "$ALICE" ] || warn "could not sign Alice in: her approval won't come, the call waits and times out"
    AS="https://as.$ALICE_DOMAIN"
    ( for _ in $(seq 1 45); do   # approve her next pending ask, if one comes
        fam=$(with_bearer "$ALICE" curl -s "${CA[@]}" "$AS/owner/pending" \
          | jq -r '(if type=="array" then . else .pending end)[0].family // empty' 2>/dev/null)
        if [ -n "$fam" ]; then
          with_bearer "$ALICE" curl -s "${CA[@]}" -X POST "$AS/owner/pending/$fam/decision" \
            -H 'content-type: application/json' -d '{"decision":"approved"}' >/dev/null
          printf '   %s(Alice approved in her portal)%s\n' "$_D" "$_N"; exit
        fi; sleep 2; done ) &
    ask "What is in Alice's portfolio? Use get_positions."
    wait $! 2>/dev/null || true
    look "Traffic: search as.alice.lab: the ticket, her terms, the grant, and Meridian checking it."
    look "Her side: https://portal.$ALICE_DOMAIN (alice / alice-demo), Settings → Security → Agent Authorization."
  fi
  next
fi

if scene 5 "An IdP outage (identity continuity)"; then
  # the active IdP, when it is one S&V reaches through its egress (an external
  # one); S&V's own are cut in their namespaces by make continuity-verify
  IDP=$(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}')
  if [ -z "$IDP" ] || ! K get serviceentry "continuity-$IDP" -n sv-egress >/dev/null 2>&1; then
    warn "no external IdP is signing people in (docs/IDPS.md): skipping"
  else
    look "Identity Continuity tab. The network to $IDP is cut now, at S&V's egress."
    on_exit "K delete authorizationpolicy continuity-partition-$IDP -n sv-egress --ignore-not-found >/dev/null 2>&1"
    K apply -f - >/dev/null <<YAML
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: continuity-partition-$IDP
  namespace: sv-egress
  labels: {continuity.lab.solo.io/tier: $IDP}
  annotations: {continuity.lab.solo.io/cut-by: make tour, continuity.lab.solo.io/path: "$(K get serviceentry "continuity-$IDP" -n sv-egress -o jsonpath='{.spec.hosts[0]}') at the sv-egress egress"}
spec:
  targetRefs: [{group: networking.istio.io, kind: ServiceEntry, name: continuity-$IDP}]
  action: DENY
  rules: [{}]
YAML
    look "Banner: amber OUTAGE while failed checks count, then red FAILOVER ACTIVE."
    t=$(date +%s); until [ "$(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}')" != "$IDP" ] || [ $(( $(date +%s) - t )) -gt 60 ]; do sleep 1; done
    say "failed over to $(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}') after $(( $(date +%s) - t ))s"
    look "A new sign-in to kagent now goes to the next IdP. Bob's sessions keep working."
    next
    K delete authorizationpolicy "continuity-partition-$IDP" -n sv-egress --ignore-not-found >/dev/null
    look "Network restored. Amber while healthy checks count, then it fails back and turns green."
    t=$(date +%s); until [ "$(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}')" = "$IDP" ] || [ $(( $(date +%s) - t )) -gt 90 ]; do sleep 1; done
    say "back on $(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}') after $(( $(date +%s) - t ))s"
  fi
  next
fi

if scene 6 "Take it with you"; then
  look "Topology: the export button (bottom left) saves the whole map, in the current view, as a PNG."
  look "Traffic: Pause freezes the list; expand any row for the parsed record or its raw JSON."
  say "make reset rewinds Alice's grants and gives Bob's agent a new key for the next run."
fi
echo; ok "tour done"
