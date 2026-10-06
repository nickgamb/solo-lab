#!/usr/bin/env bash
# interop-keys: the public keys S&V gives a partner's authorization server,
# into .lab/interop/keys/ (both are stable across rebuilds):
#   sv-client.jwks.json  S&V's client key (private_key_jwt, client
#                        sterling-vance-kagent): register it for S&V's client
#   sv-idp.jwks.json     S&V's Keycloak signing keys, issuer
#                        https://idp.sterling.lab/realms/sterling-vance: trust
#                        them for ID-JAGs S&V's Keycloak issues (that issuer
#                        is not reachable from the internet)
. "$(dirname "$0")/lib.sh"
. "$LAB_ROOT/scripts/idp.sh"
need_cluster
out="$LAB_STATE/interop/keys"; mkdir -p "$out"
realm_signing_key sv-xaa-client
xaa_client_jwks >"$out/sv-client.jwks.json"
lp=$(free_port); port_forward sv-identity keycloak "$lp" 80
curl -sf "http://127.0.0.1:$lp/realms/sterling-vance/protocol/openid-connect/certs" \
  | jq '{keys: [.keys[] | select(.use == "sig")]}' >"$out/sv-idp.jwks.json"
ok "client $SV_CLIENT_AT_LEDGERLINE, kid $(jq -r '.keys[0].kid' "$out/sv-client.jwks.json"): $out/sv-client.jwks.json"
ok "issuer $SV_ISSUER: $out/sv-idp.jwks.json"
