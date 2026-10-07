#!/usr/bin/env bash
# Story 1 enforcement checks, from real workload identities in the mesh.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster; need_password_grant
# Bob's access token as the edge forwards it after SSO (kagent)
BOB=$(user_token sv-identity sterling-vance kagent "$(lab_secret SV_KAGENT_CLIENT_SECRET)" bob bob-demo)
[ -n "$BOB" ] && [ "$BOB" != null ] || die "could not get Bob's token"
# someone else's ID token (ops, a platform admin in S&V's realm), to try to swap in
OTHER_ID=$(kc_token sv-identity sterling-vance kagent "$(lab_secret SV_KAGENT_CLIENT_SECRET)" ops ops-demo | jq -r .id_token)
# Bob's agent's workload identity (its worker pool's ServiceAccount), another
# workload in the same namespace, and one in another namespace.
probe_pod sv-agents bob-assistant; probe_pod sv-agents; probe_pod observability; probe_pod kagent kagent-ui
AGENT=sv-agents/probe-bob-assistant
# Agents call the tool's own Service; the mesh carries every call into sv-mcp's
# agentgateway waypoint. There is no gateway URL to remember, or to skip.
GW=http://bob-workspace-mcp.sv-mcp:3000/mcp
POD_IP=$(K get pod -n sv-mcp -l app.kubernetes.io/name=bob-workspace -o jsonpath='{.items[0].status.podIP}')
pass=0 fail=0
check() {  # check <expect-regex> <label> <ns>[/<pod>] <probe args...>
  local want=$1 label=$2 ns=${3%%/*} pod=probe; [[ $3 == */* ]] && pod=${3#*/}; shift 3
  local out; out=$(probe_exec "$ns/$pod" "$@" 2>&1 | tail -1 || true)
  if echo "$out" | grep -qE "$want"; then ok "$label"; pass=$((pass+1))
  else warn "$label"; echo "      got: ${out:0:300}"; fail=$((fail+1)); fi
}
step "Allowed: an S&V agent workload acting for Bob"
check '"whoami"'                                "agent lists Bob's tools (as Bob)"            $AGENT $GW list --token "$BOB"
check '\\"acting_for\\": \\"bob\\".*\\"audience\\": \\"bob-workspace\\"' \
                                                "whoami: acts as bob, token aud=bob-workspace" $AGENT $GW call whoami '{}' --token "$BOB"
check 'Alice Chen'                              "list_clients returns Bob's book"             $AGENT $GW call list_clients '{}' --token "$BOB"
step "Refused"
check 'Unknown tool|isError": true|http": 40[13]' "export_book: hidden from advisors (compliance only)"               $AGENT $GW call export_book '{}' --token "$BOB"
check 'http": 40[13]|isError": true'            "agent with no user token (discovery lane is controller-only)" $AGENT $GW call whoami '{}'
check 'http": 40[13]|isError": true'            "right user, wrong workload (observability)"  observability $GW call whoami '{}' --token "$BOB"
check 'http": 40[13]|isError": true'            "right user, not an agent (another sv-agents workload)" sv-agents $GW call whoami '{}' --token "$BOB"
check 'http": 40[13]|RBAC|isError": true|refused|reset|Broken pipe|Connection' \
                                                "skip the waypoint: dial a pod IP with Bob's token" $AGENT "http://$POD_IP:3000/mcp" call whoami '{}' --token "$BOB"
XAA=http://ai-gateway.agentgateway-system/xaa/ledgerline/mcp
# Ledgerline's account for Bob: its own user ID (RESOURCE_AS=keycloak), or a
# token its Gluu issued for Bob (RESOURCE_AS=gluu)
. "$(dirname "$0")/../../scripts/idp.sh"
ras_env
ACCOUNT='11ed0000-0000-4000-8000-000000000b0b' CHAT='11ed0000-0000-4000-8000-000000000b0b|bob@sterling\.lab'
if [ "$RESOURCE_AS" = gluu ]; then
  ISS=$(echo "${RESOURCE_AS_ISSUER%/}" | sed 's/[.]/\\./g')
  ACCOUNT="issuer[\\\"]*: [\\\"]*$ISS.*ledgerline_account[\\\"]*: [\\\"]*bob|ledgerline_account[\\\"]*: [\\\"]*bob.*issuer[\\\"]*: [\\\"]*$ISS"
  CHAT='bob'
fi
# the IdP that should vouch for Bob: his session's (the token's idp claim)
# when it issues ID-JAGs and is S&V's active tier, else S&V's Keycloak. These
# checks sign Bob in with S&V's own login; a browser session through Gluu
# has Gluu vouch.
SESSION_IDP=$(echo "$BOB" | cut -d. -f2 | python3 -c 'import base64,json,sys; s=sys.stdin.read().strip(); print(json.loads(base64.urlsafe_b64decode(s+"="*(-len(s)%4))).get("idp",""))')
ACTIVE=$(K get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}' 2>/dev/null)
VOUCHER=keycloak
if [ -n "$SESSION_IDP" ] && [ "$SESSION_IDP" = "$ACTIVE" ]; then
  case " $(idp_xaa_upstreams) " in *" $SESSION_IDP "*) VOUCHER=$SESSION_IDP ;; esac
fi
step "Cross App Access: Bob's agent -> Ledgerline Research (ID-JAG)"
check "$ACCOUNT"                                "account_info: Ledgerline's own account for Bob"  $AGENT $XAA call account_info '{}' --token "$BOB"
# xaa-relay's record of the last ID-JAG it accepted (the gateway caches
# Ledgerline's token for up to five minutes)
vouched=$(K logs -n agentgateway-system -l app=xaa-relay --since=6m --tail=-1 2>/dev/null \
  | grep '"msg":"id-jag accepted"' | jq -rs 'sort_by(.time) | last | "\(.idp) iss=\(.claims.iss) aud=\(.claims.aud) typ=\(.header.typ)"' 2>/dev/null)
if [ "${vouched%% *}" = "$VOUCHER" ]; then ok "ID-JAG from $VOUCHER, checked at S&V's egress: ${vouched#* }"; pass=$((pass+1))
else
  warn "ID-JAG from $VOUCHER, checked at S&V's egress"; echo "      got: ${vouched:-no ID-JAG in the xaa-relay log}"
  [ "$VOUCHER" != keycloak ] && echo "      $VOUCHER vouches for a session through it once Bob has signed in to Ledgerline through it (https://idp.$LEDGERLINE_DOMAIN/realms/ledgerline/account)"
  fail=$((fail+1))
fi
check 'overweight'                              "sector_outlook through XAA"                       $AGENT $XAA call sector_outlook '{"sector":"technology"}' --token "$BOB"
check "$ACCOUNT"                                "another user's ID token is replaced: still Bob"   $AGENT $XAA call account_info '{}' --token "$BOB" --header "x-id-token=$OTHER_ID"
# Ledgerline's server records any ID token that reaches it: none may
leaked=$(K logs -n ledgerline -l app.kubernetes.io/name=ledgerline-research --since=10m --tail=-1 2>/dev/null | grep -c '"event": "unexpected credential header"' || true)
if [ "${leaked:-0}" = 0 ]; then ok "Ledgerline never receives an ID token"; pass=$((pass+1))
else warn "Ledgerline never receives an ID token"; echo "      got: $leaked requests carrying x-id-token"; fail=$((fail+1)); fi
check 'http": 40[13]'                           "right token, wrong workload (observability)"      observability $XAA call account_info '{}' --token "$BOB"
check 'http": 40[13]|refused|reset|Broken pipe|Connection' \
                                                "ask the ID-token exchange directly (ai-gateway only)" $AGENT http://idtoken-exchange.agentgateway-system:8080/mcp call account_info '{}' --token "$BOB"
check 'http": 40[13]|RBAC'                      "straight to Ledgerline with Bob's S&V token"      $AGENT https://mcp.ledgerline.lab/mcp call account_info '{}' --token "$BOB"
# and through Bob's agent itself, as in the chat (the model picks the tool)
ask='{jsonrpc:"2.0",id:"1",method:"message/send",params:{message:{role:"user",kind:"message",messageId:(now|tostring),contextId:$c,parts:[{kind:"text",text:"Which Ledgerline account am I using? Use account_info."}]}}}'
out=$(a2a_send "$BOB" "$(jq -nc --arg c "$(new_uuid)" "$ask")" | jq -r '[.result.history[]? | select(.role=="agent") | .parts[]? | .text // empty] | last // "no reply"' 2>&1)
if echo "$out" | grep -qE "$CHAT"; then ok "Bob's agent, in chat: Ledgerline's own account for Bob"; pass=$((pass+1))
else warn "Bob's agent, in chat: Ledgerline's own account for Bob"; echo "      got: ${out:0:300}"; fail=$((fail+1)); fi

echo; [ $fail -eq 0 ] && ok "story 1: $pass/$((pass+fail)) checks passed" || die "story 1: $fail of $((pass+fail)) checks failed"
