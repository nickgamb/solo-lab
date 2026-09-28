#!/usr/bin/env bash
# Story 1 enforcement checks, from real workload identities in the mesh.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster
# Bob's session as the edge sees it after SSO: access token + ID token (kagent)
lp=$((18000 + RANDOM % 1000)); port_forward sv-identity keycloak "$lp" 80
TOK=$(curl -s "http://127.0.0.1:$lp/realms/sterling-vance/protocol/openid-connect/token" -d grant_type=password \
  -d client_id=kagent -d client_secret="$(lab_secret SV_KAGENT_CLIENT_SECRET)" -d username=bob -d password=bob-demo -d scope=openid)
BOB=$(echo "$TOK" | jq -r .access_token); BOB_ID=$(echo "$TOK" | jq -r .id_token); unset TOK
[ -n "$BOB" ] && [ "$BOB" != null ] || die "could not get Bob's token"
probe_pod sv-agents; probe_pod observability
# Agents call the tool's own Service; the mesh carries every call into sv-mcp's
# agentgateway waypoint. There is no gateway URL to remember, or to skip.
GW=http://bob-workspace-mcp.sv-mcp:3000/mcp
POD_IP=$(K get pod -n sv-mcp -l app.kubernetes.io/name=bob-workspace -o jsonpath='{.items[0].status.podIP}')
pass=0 fail=0
check() {  # check <expect-regex> <label> <ns> <probe args...>
  local want=$1 label=$2 ns=$3; shift 3
  local out; out=$(K exec -n "$ns" probe -- python3 /tmp/p.py "$@" 2>&1 | tail -1 || true)
  if echo "$out" | grep -qE "$want"; then ok "$label"; pass=$((pass+1))
  else warn "$label"; echo "      got: ${out:0:300}"; fail=$((fail+1)); fi
}
step "Allowed: an S&V agent workload acting for Bob"
check '"whoami"'                                "agent lists Bob's tools (as Bob)"            sv-agents $GW list --token "$BOB"
check '\\"acting_for\\": \\"bob\\".*\\"audience\\": \\"bob-workspace\\"' \
                                                "whoami: acts as bob, token aud=bob-workspace" sv-agents $GW call whoami '{}' --token "$BOB"
check 'Alice Chen'                              "list_clients returns Bob's book"             sv-agents $GW call list_clients '{}' --token "$BOB"
step "Refused"
check 'Unknown tool|isError": true|http": 40[13]' "export_book: hidden from advisors (compliance only)"               sv-agents $GW call export_book '{}' --token "$BOB"
check 'http": 40[13]|isError": true'            "agent with no user token (discovery lane is controller-only)" sv-agents $GW call whoami '{}'
check 'http": 40[13]|isError": true'            "right user, wrong workload (observability)"  observability $GW call whoami '{}' --token "$BOB"
check 'http": 40[13]|RBAC|isError": true|refused|reset|Broken pipe|Connection' \
                                                "skip the waypoint: dial a pod IP with Bob's token" sv-agents "http://$POD_IP:3000/mcp" call whoami '{}' --token "$BOB"
XAA=http://ai-gateway.agentgateway-system/xaa/ledgerline/mcp
step "Cross App Access: Bob's agent -> Ledgerline Research (ID-JAG)"
check '11ed0000-0000-4000-8000-000000000b0b'   "account_info: Ledgerline's own account for Bob"  sv-agents $XAA call account_info '{}' --token "$BOB" --header "x-id-token=$BOB_ID"
check 'overweight'                              "sector_outlook through XAA"                       sv-agents $XAA call sector_outlook '{"sector":"technology"}' --token "$BOB" --header "x-id-token=$BOB_ID"
check 'http": 40[13]'                           "no ID token: refused at the egress gateway"       sv-agents $XAA call account_info '{}' --token "$BOB"
check 'http": 40[13]'                           "right tokens, wrong workload (observability)"     observability $XAA call account_info '{}' --token "$BOB" --header "x-id-token=$BOB_ID"
check 'http": 40[13]|RBAC'                      "straight to Ledgerline with Bob's S&V token"      sv-agents https://mcp.ledgerline.lab/mcp call account_info '{}' --token "$BOB"

echo; [ $fail -eq 0 ] && ok "story 1: $pass/$((pass+fail)) checks passed" || die "story 1: $fail of $((pass+fail)) checks failed"
