#!/usr/bin/env bash
# An outside MCP server at S&V's front door, behind its own authorization
# server (Enterprise: agentgateway's OAuth issuer, brokered elicitation).
#
#   upstream-mcp.sh add <name> <mcp-url> <as-url> [scope...]
#   upstream-mcp.sh remove <name>
#   upstream-mcp.sh list
#
# A client signs in at S&V's broker as at any front-door resource
# (MCP_PUBLIC_URL/mcp/<name>); the issuer then signs the user in at <as-url>
# once and keeps that token for the user, and the gateway sends it on each
# call to <mcp-url>. The issuer names itself there by its own client ID
# metadata document (MCP_UPSTREAM_CIMD=true, when the server supports them),
# else registers itself dynamically.
. "$(dirname "$0")/lib.sh"
. "$LAB_ROOT/scripts/idp.sh"
need_cluster
[ "$AGW_EDITION" = enterprise ] || die "an outside MCP server behind its own authorization server needs agentgateway Enterprise (the OAuth issuer)"
T="$LAB_ROOT/demos/bob/manifests/upstream/resource.yaml"
L=lab.solo.io/upstream-resource

case "${1:-}" in
  add)
    [ $# -ge 4 ] || die "usage: upstream-mcp.sh add <name> <mcp-url> <as-url> [scope...]"
    name=$2 url=$3 as=$4; shift 4
    [[ $name =~ ^[a-z0-9][a-z0-9-]{0,40}$ ]] || die "name: lower case letters, digits and -"
    [[ $url =~ ^https://([^/:]+)(/.*)?$ ]] || die "the MCP server's URL must be https://host[/path]"
    export UPSTREAM_MCP_HOST=${BASH_REMATCH[1]} UPSTREAM_MCP_PATH=${BASH_REMATCH[2]:-/}
    [[ $as =~ ^https://[^/]+ ]] || die "the authorization server's URL must be https://"
    export UPSTREAM_RESOURCE=$name UPSTREAM_NAME=$name UPSTREAM_AS=${as%/}
    export UPSTREAM_SCOPES UPSTREAM_RESOURCE_METADATA
    UPSTREAM_SCOPES=$(jq -nc '$ARGS.positional' --args "$@")
    UPSTREAM_RESOURCE_METADATA=$(mcp_issuer_metadata "$name")
    apply_tmpl "$T"
    accepted() { [ "$(K get "$AGW_POLICY_KIND" "upstream-$name" -n agentgateway-system -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}')" = True ]; }
    wait_for "upstream-$name accepted" 20 3 accepted
    ok "$MCP_PUBLIC_URL/mcp/$name -> $url (signed in at $UPSTREAM_AS$([ $# -gt 0 ] && echo ", scopes: $*"))"
    ;;
  remove)
    [ -n "${2:-}" ] || die "usage: upstream-mcp.sh remove <name>"
    for kind in httproute "$AGW_POLICY_KIND" "$AGW_BACKEND_KIND"; do
      K delete "$kind" -n agentgateway-system -l "$L=$2" --ignore-not-found >/dev/null
    done
    ok "removed /mcp/$2"
    ;;
  list)
    K get "$AGW_BACKEND_KIND" -n agentgateway-system -l "$L" \
      -o jsonpath='{range .items[*]}{.metadata.labels.lab\.solo\.io/upstream-resource}{"\t"}{.spec.mcp.targets[0].static.host}{.spec.mcp.targets[0].static.path}{"\n"}{end}'
    ;;
  *) die "usage: upstream-mcp.sh add <name> <mcp-url> <as-url> [scope...] | remove <name> | list" ;;
esac
