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
#
# CLIENT_DOC=<doc>: instead, a client ID metadata document of the gateway's
# own for that server, at MCP_PUBLIC_URL/clients/<doc>.json (or at <doc>,
# when it is a path): the client_id the issuer signs in with there, with the
# issuer's redirect URI and the scopes. The server's endpoints come from its
# metadata. CLIENT_AUTH=private_key_jwt: the issuer authenticates with an
# ES256 key of its own (.lab/keys/upstream-client-<doc>.key), whose public key
# the document names (jwks_uri); none (the default): a public client. Either
# way PKCE S256. The server fetches the document, so MCP_PUBLIC_URL must be a
# public https URL it can reach.
. "$(dirname "$0")/lib.sh"
. "$LAB_ROOT/scripts/idp.sh"
need_cluster
[ "$AGW_EDITION" = enterprise ] || die "an outside MCP server behind its own authorization server needs agentgateway Enterprise (the OAuth issuer)"
T="$LAB_ROOT/demos/bob/manifests/upstream/resource.yaml"
L=lab.solo.io/upstream-resource

# as_metadata <as-url>: the authorization server's metadata (RFC 8414, then
# OpenID discovery; for an issuer with a path, both places either spec puts it)
as_metadata() {
  local as=$1 origin path u m
  [[ $as =~ ^(https://[^/]+)(/.*)?$ ]] && origin=${BASH_REMATCH[1]} path=${BASH_REMATCH[2]:-}
  for u in "$origin/.well-known/oauth-authorization-server$path" "$as/.well-known/oauth-authorization-server" \
           "$as/.well-known/openid-configuration" "$origin/.well-known/openid-configuration$path"; do
    m=$(curl -sf -m 15 --cacert "$LAB_TRUST" "$u" 2>/dev/null) || continue
    jq -e '.authorization_endpoint and .token_endpoint' <<<"$m" >/dev/null 2>&1 && { echo "$m"; return 0; }
  done
  return 1
}

# client_doc <name> <doc> <none|private_key_jwt> <scopes>: the gateway's own
# client ID metadata document for <name>'s authorization server, and the
# issuer's client there (UPSTREAM_OAUTH)
client_doc() {
  local name=$1 doc=$2 auth=$3 scopes=$4 m path id key crt body jwks sec
  case "$auth" in none|private_key_jwt) ;; *) die "CLIENT_AUTH: none or private_key_jwt" ;; esac
  [[ $MCP_PUBLIC_URL =~ ^https://([^/]+)$ ]] || die "CLIENT_DOC: MCP_PUBLIC_URL must be https://<host> (now $MCP_PUBLIC_URL)"
  export UPSTREAM_CLIENT_HOST=${BASH_REMATCH[1]}
  [[ $UPSTREAM_CLIENT_HOST == *.lab ]] && warn "$MCP_PUBLIC_URL is a .lab name: an outside authorization server can't fetch the document"
  if [[ $doc == /* ]]; then path=$doc; else
    [[ $doc =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "CLIENT_DOC: a name (letters, digits, . _ -) or a path"
    path=/clients/$doc.json
  fi
  export UPSTREAM_CLIENT_PATH=$path UPSTREAM_CLIENT_JWKS_PATH=${path%.json}/jwks.json
  export UPSTREAM_CLIENT_ID=$MCP_PUBLIC_URL$path
  m=$(as_metadata "$UPSTREAM_AS") || die "no authorization server metadata at $UPSTREAM_AS"
  body=$(jq -nc --arg id "$UPSTREAM_CLIENT_ID" --arg r "$MCP_PUBLIC_URL/oauth-issuer/callback/upstream" \
    --arg s "$scopes" --arg a "$auth" --arg j "$MCP_PUBLIC_URL$UPSTREAM_CLIENT_JWKS_PATH" \
    '{client_id: $id, client_name: "Sterling & Vance (agentgateway)", redirect_uris: [$r],
      grant_types: ["authorization_code", "refresh_token"], response_types: ["code"], token_endpoint_auth_method: $a}
     + (if $s != "" then {scope: $s} else {} end)
     + (if $a == "private_key_jwt" then {token_endpoint_auth_signing_alg: "ES256", jwks_uri: $j} else {} end)')
  export UPSTREAM_CLIENT_DOC_BODY UPSTREAM_CLIENT_JWKS_BODY
  UPSTREAM_CLIENT_DOC_BODY=$(jq -c 'tojson' <<<"$body")
  UPSTREAM_OAUTH=$(jq -nc --argjson m "$m" --arg id "$UPSTREAM_CLIENT_ID" --argjson s "$UPSTREAM_SCOPES" --arg n "$UPSTREAM_NAME" \
    '{clientId: $id, clientName: $n, authorizeUrl: $m.authorization_endpoint, accessTokenUrl: $m.token_endpoint,
      scopes: $s, pkce: "Always"}')
  sec=upstream-$name-client
  if [ "$auth" = private_key_jwt ]; then
    # its key: kept in .lab/keys, so the public key it registers stays the same
    id=${path##*/}; id=upstream-client-${id%.json}
    key="$LAB_STATE/keys/$id.key" crt="$LAB_STATE/keys/$id.crt"
    mkdir -p "$LAB_STATE/keys"; chmod 700 "$LAB_STATE/keys"
    if [ ! -s "$key" ]; then
      openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 -subj "/CN=$id" \
        -keyout "$key" -out "$crt" >/dev/null 2>&1 || die "making $key"
      chmod 600 "$key"
    fi
    K create secret tls "$sec" -n agentgateway-system --cert="$crt" --key="$key" --dry-run=client -o yaml \
      | K label --local -f - "$L=$name" -o yaml | K apply -f - >/dev/null
    # the JWK: the key's point, the certificate, and its SHA-256 thumbprint as kid
    jwks=$(openssl x509 -in "$crt" -outform DER | python3 -c '
import base64, hashlib, json, subprocess, sys
der = sys.stdin.buffer.read()
spki = subprocess.run(["openssl", "x509", "-inform", "DER", "-pubkey", "-noout"], input=der, capture_output=True).stdout
pub = subprocess.run(["openssl", "pkey", "-pubin", "-outform", "DER"], input=spki, capture_output=True).stdout
pt = pub[-65:]                       # uncompressed point: 04 || x || y
b = lambda x: base64.urlsafe_b64encode(x).rstrip(b"=").decode()
t = b(hashlib.sha256(der).digest())
print(json.dumps({"keys": [{"kty": "EC", "crv": "P-256", "x": b(pt[1:33]), "y": b(pt[33:]), "alg": "ES256", "use": "sig",
                            "kid": t, "x5t#S256": t, "x5c": [base64.b64encode(der).decode()]}]}))')
    UPSTREAM_CLIENT_JWKS_BODY=$(jq -c 'tojson' <<<"$jwks")
    UPSTREAM_OAUTH=$(jq -c --arg sec "$sec" '. + {certificateKeyPairRef: {name: $sec}}' <<<"$UPSTREAM_OAUTH")
    apply_tmpl "$LAB_ROOT/demos/bob/manifests/upstream/client-jwks.yaml"
  else
    K delete secret "$sec" -n agentgateway-system --ignore-not-found >/dev/null
    K delete httproute,directresponses.gateway.kgateway.dev "upstream-$name-client-jwks" -n agentgateway-system --ignore-not-found >/dev/null 2>&1
  fi
  apply_tmpl "$LAB_ROOT/demos/bob/manifests/upstream/client-document.yaml"
  ok "client ID metadata document: $UPSTREAM_CLIENT_ID ($auth$([ "$auth" = private_key_jwt ] && echo ", keys at $MCP_PUBLIC_URL$UPSTREAM_CLIENT_JWKS_PATH"))"
}

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
    export UPSTREAM_OAUTH
    if [ -z "${CLIENT_DOC:-}" ]; then
      UPSTREAM_OAUTH=$(jq -nc --arg as "$UPSTREAM_AS" --argjson s "$UPSTREAM_SCOPES" --arg n "$UPSTREAM_NAME" \
        '{baseUrl: $as, scopes: $s, clientName: $n}')
      apply_tmpl "$T"
      K delete secret "upstream-$name-client" -n agentgateway-system --ignore-not-found >/dev/null
      K delete httproute,directresponses.gateway.kgateway.dev -n agentgateway-system \
        "upstream-$name-client" "upstream-$name-client-jwks" --ignore-not-found >/dev/null 2>&1
    else
      client_doc "$name" "$CLIENT_DOC" "${CLIENT_AUTH:-none}" "$*"
      apply_tmpl "$T"
    fi
    accepted() { [ "$(K get "$AGW_POLICY_KIND" "upstream-$name" -n agentgateway-system -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}')" = True ]; }
    wait_for "upstream-$name accepted" 20 3 accepted
    ok "$MCP_PUBLIC_URL/mcp/$name -> $url (signed in at $UPSTREAM_AS$([ $# -gt 0 ] && echo ", scopes: $*"))"
    ;;
  remove)
    [ -n "${2:-}" ] || die "usage: upstream-mcp.sh remove <name>"
    for kind in httproute "$AGW_POLICY_KIND" "$AGW_BACKEND_KIND" directresponses.gateway.kgateway.dev; do
      K delete "$kind" -n agentgateway-system -l "$L=$2" --ignore-not-found >/dev/null 2>&1
    done
    K delete secret "upstream-$2-client" -n agentgateway-system --ignore-not-found >/dev/null
    ok "removed /mcp/$2"
    ;;
  list)
    K get "$AGW_BACKEND_KIND" -n agentgateway-system -l "$L" \
      -o jsonpath='{range .items[*]}{.metadata.labels.lab\.solo\.io/upstream-resource}{"\t"}{.spec.mcp.targets[0].static.host}{.spec.mcp.targets[0].static.path}{"\n"}{end}'
    ;;
  *) die "usage: upstream-mcp.sh add <name> <mcp-url> <as-url> [scope...] | remove <name> | list" ;;
esac
