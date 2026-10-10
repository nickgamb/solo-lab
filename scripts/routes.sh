#!/usr/bin/env bash
# Where sign-ins go now: each routing rule's IdP (the IdentityContinuity's
# status, as written into the gateway), then three sign-ins sent through the
# edge, as a browser app, an AI client, and an AI client for Ledgerline, and
# the IdP each one lands at. No credentials: it stops at the IdP's door.
. "$(dirname "$0")/lib.sh"
need_cluster
idc=$(K get idc sterling-vance -n sv-identity -o json)
step "Routing rules (spec.routing), first match wins"
echo "$idc" | jq -r '(.status.routing // []) as $s | .status.active as $a
  | (.spec.routing.rules // [])[] | . as $r | ([$s[] | select(.name == $r.name)][0]) as $now
  | "  \($r.name) -> \($now.idp // ($a + " (active)"))   \($now.reason // "not applied yet")\n      when \($r.when)"'
echo "  everything else -> $(echo "$idc" | jq -r '.status.active') (active)"
step "Sign-ins, through the edge"
A="https://idp.$SV_DOMAIN/realms/sterling-vance/protocol/openid-connect/auth?response_type=code&scope=openid&code_challenge=vErIfYvErIfYvErIfYvErIfYvErIfYvErIfYvErIf00&code_challenge_method=S256&state=routes"
lands() {
  local loc; loc=$(curl -s --cacert "$LAB_CA_DIR/ca.crt" -o /dev/null -w '%{redirect_url}' "$A&$2")
  case "$loc" in
    */broker/*/login*) loc=${loc#*/broker/}; printf '  %-34s -> %s\n' "$1" "${loc%%/*}" ;;
    "") printf '  %-34s -> %s\n' "$1" "the broker's own sign-in form" ;;
    *) printf '  %-34s -> %s\n' "$1" "${loc%%\?*}" ;;
  esac
}
lands "a browser app (kagent)" "client_id=kagent&redirect_uri=https%3A%2F%2Fkagent.$SV_DOMAIN%2Foauth2%2Fredirect"
lands "an AI client (sv-mcp-client)" "client_id=sv-mcp-client&redirect_uri=http%3A%2F%2F127.0.0.1%3A33418%2Fcallback"
lands "an AI client, for Ledgerline" "client_id=sv-mcp-client&redirect_uri=http%3A%2F%2F127.0.0.1%3A33418%2Fcallback&resource=https%3A%2F%2Fmcp.$SV_DOMAIN%2Fmcp%2Fledgerline"
