#!/usr/bin/env bash
# xaa-logs [--since 1h]: the Cross App Access trail, into
# .lab/xaa/<UTC time>/. Every JWT is replaced by <jwt>; claims logged in place
# of tokens are kept.
#
#   ai-gateway.log          agentgateway access log, XAA routes: the MCP
#                           calls (caller SPIFFE id, verified token claims,
#                           MCP method and tool) and both token requests the
#                           gateway made (xaa-idp-<idp>: the verified ID
#                           token's claims; xaa-as-ledgerline: the verified
#                           ID-JAG's claims), status and refusals
#   idtoken-exchange.log    the ID token each ID-JAG request started from
#   ledgerline-mcp.log      the access token Ledgerline accepted or refused
#   ledgerline-waypoint.log Ledgerline's waypoint access log
#   xaa-config.yaml         each IdP backend's crossAppAccess policy
#   discovery-*.json        the enterprise IdPs' and Ledgerline AS's discovery
. "$(dirname "$0")/lib.sh"
. "$LAB_ROOT/scripts/idp.sh"
need_cluster
since=1h; [ "${1:-}" = --since ] && since=${2:?--since <duration>}
out="$LAB_STATE/xaa/$(date -u +%Y%m%dT%H%M%SZ)"; mkdir -p "$out"
redact() { sed -E 's/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/<jwt>/g'; }
logs() {  # logs <ns> <selector> [grep -E filter]
  K logs -n "$1" -l "$2" --all-containers --prefix --timestamps --since="$since" --tail=-1 2>/dev/null \
    | { if [ -n "${3:-}" ]; then grep -E "$3" || true; else cat; fi; } | redact
}
logs agentgateway-system app=idtoken-exchange >"$out/idtoken-exchange.log"
logs agentgateway-system app.kubernetes.io/name=ai-gateway 'route=agentgateway-system/xaa-' >"$out/ai-gateway.log"
logs ledgerline app.kubernetes.io/name=ledgerline-research '"event": "token' >"$out/ledgerline-mcp.log"
logs ledgerline gateway.networking.k8s.io/gateway-name=waypoint >"$out/ledgerline-waypoint.log"
K get "$AGW_BACKEND_KIND" -n agentgateway-system -l lab.solo.io/xaa-idp -o yaml 2>/dev/null \
  | yq '[.items[] | {"name": .metadata.name, "crossAppAccess": .spec.mcp.targets[0].static.policies.auth.crossAppAccess}]' >"$out/xaa-config.yaml"
# an IdP that doesn't answer is noted, and the rest of the trail still written
for n in $(idp_xaa_upstreams); do
  (idp_discover "$(_idp_var "$n" ISSUER)") >"$out/discovery-$n.json" || warn "no discovery from $n: $out/discovery-$n.json is empty"
done
if ras_external; then
  (idp_discover "$LEDGERLINE_AS_ISSUER") >"$out/discovery-resource-as.json" || warn "no discovery from Ledgerline's AS: $out/discovery-resource-as.json is empty"
fi
for f in "$out"/*; do printf '  %-28s %s lines\n' "$(basename "$f")" "$(wc -l <"$f" | tr -d ' ')"; done
ok "Cross App Access trail: $out"
