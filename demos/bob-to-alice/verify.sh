#!/usr/bin/env bash
# Story 2 checks. Drives the real path (kagent -> bob-assistant -> U4A adapter
# -> edge -> Meridian -> Alice's AS) and plays Alice's decisions through her
# owner API; in the demo she clicks them in her portal. Run after `make reset`.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster; need_password_grant
pass=0 fail=0
res() { if [ "$1" = ok ]; then ok "$2"; pass=$((pass+1)); else warn "$2"; echo "      got: ${3:0:300}"; fail=$((fail+1)); fi; }
expect() { echo "$3" | tr '\n' ' ' | grep -qiE "$1" && res ok "$2" || res no "$2" "$(echo "$3" | tr '\n' ' ')"; }

. "$LAB_ROOT/scripts/idp.sh"
prefer_tier keycloak   # Bob signs in through S&V's own IdP, which a script can drive
BOB=$(sso_token bob bob-demo | jq -r .access_token)
ALICE=$(user_token alice-identity alice alice-portal "" alice alice-demo)
[ -n "$BOB" ] && [ -n "$ALICE" ] || die "sign-in failed"
probe_pod kagent kagent-ui; probe_pod sv-agents
AS="https://as.$ALICE_DOMAIN"; CA=(--cacert "$LAB_CA_DIR/ca.crt")

ask_bob() {  # ask_bob <question> -> the agent's last reply (what the UI shows), in a new session
  local body; body=$(jq -nc --arg q "$1" --arg c "$(new_uuid)" '{jsonrpc:"2.0",id:"1",method:"message/send",params:{message:{role:"user",kind:"message",messageId:(now|tostring),contextId:$c,parts:[{kind:"text",text:$q}]}}}')
  a2a_send "$BOB" "$body" | jq -r '[.result.history[]? | select(.role=="agent") | .parts[]? | .text // empty] | last // "no reply"'
}
alice_decides() {  # alice_decides approved|denied: waits for her next pending ask
  local p fam kind i
  ALICE=$(user_token alice-identity alice alice-portal "" alice alice-demo)   # 5-minute tokens
  for i in $(seq 1 60); do
    p=$(with_bearer "$ALICE" curl -s "${CA[@]}" "$AS/owner/pending")
    fam=$(echo "$p" | jq -r '(if type=="array" then . else .pending end)[0].family // empty' 2>/dev/null)
    [ -n "$fam" ] && break; sleep 2
  done
  [ -n "$fam" ] || { echo "none"; return; }
  kind=$(echo "$p" | jq -r '(if type=="array" then . else .pending end)[0] | "\(.kind)/\(.tier // .tier_id)"')
  with_bearer "$ALICE" curl -s "${CA[@]}" -X POST "$AS/owner/pending/$fam/decision" \
    -H 'content-type: application/json' -d "{\"decision\":\"$1\"}" >/dev/null
  echo "$kind"
}
ask_with_alice() {  # ask_with_alice <decision> <question> -> "<pend kind> | <reply>"
  local out pend; out=$(mktemp)
  ask_bob "$2" > "$out" & local pid=$!
  pend=$(alice_decides "$1"); wait $pid
  echo "$pend | $(cat "$out")"; rm -f "$out"
}

step "Tier 1: holdings. A first contact waits for Alice"
r=$(ask_with_alice approved "What is in Alice's portfolio? Use get_positions.")
expect '^connection/tier1 .*VTI' "held for Alice, approved, holdings returned" "$r"

step "Tier 2: transactions. A new tier asks again, then her terms cover it"
r=$(ask_with_alice approved "Show Alice's transaction history. Use get_transactions with no account argument.")
expect 'tier2.*(buy|sell|dividend|VTI|AAPL)' "held at a new tier, approved, history returned" "$r"
r=$(ask_bob "Show Alice's transaction history again. Use get_transactions with no account argument.")
expect '(buy|sell|dividend|VTI|AAPL)' "same ask again: through on her standing terms" "$r"

step "Tier 3: a trade always asks, and she can say no"
r=$(ask_with_alice denied "Sell 200 shares of Alice's AAPL. Use execute_trade with symbol AAPL, side sell, quantity 200.")
expect '^operation/tier3 ' "held per operation, and Alice denied it" "$r"
# what happened, from Alice's own ledger rather than the model's words: her
# denial is recorded and the trade never touched her account
ALICE=$(user_token alice-identity alice alice-portal "" alice alice-demo)
l=$(with_bearer "$ALICE" curl -s "${CA[@]}" "$AS/owner/ledger" | jq -r '[.[] | select(.tier == "tier3")] | group_by(.family)
  | max_by(map(.ts) | max) // [] | map(.kind) | if index("denied") and (index("touched") | not) then "denied, never executed" else join(",") end')
expect '^denied, never executed' "Alice's ledger: the trade was denied and never executed" "$l"

step "Refused (real identities, no model involved)"
c=$(curl -s -o /dev/null -w '%{http_code}' "${CA[@]}" -X POST "https://gateway.$MERIDIAN_DOMAIN/mcp" -H 'content-type: application/json' \
  -H 'mcp-method: tools/call' -H 'mcp-name: get_positions' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_positions","arguments":{}}}')
expect '401' "no grant: Meridian answers 401 + UMA challenge" "$c"
c=$(curl -s -o /dev/null -w '%{http_code}' "${CA[@]}" "$AS/owner/pending")
expect '401|403' "Alice's owner API without her token" "$c"
o=$(probe_exec sv-agents http://u4a-adapter.sv-u4a:9030/mcp list 2>&1 | tail -1)
expect 'connection failed|http": 0' "another S&V workload can't use Bob's agent's adapter" "$o"
o=$(probe_exec sv-agents http://alice-vault.meridian:9020/mcp list 2>&1 | tail -1)
expect 'connection failed|http": 0' "straight to Alice's vault, skipping Meridian's gateway" "$o"
o=$(K exec -n sv-agents probe -- curl -s -m 5 -o /dev/null -w '%{http_code}' http://uma-as.alice:9000/owner/pending 2>&1 || true)
expect '000|403|503' "straight to Alice's AS, skipping the edge" "$o"

echo; [ $fail -eq 0 ] && ok "story 2: $pass/$((pass+fail)) checks passed" || die "story 2: $fail of $((pass+fail)) checks failed"
