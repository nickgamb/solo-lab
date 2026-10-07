#!/usr/bin/env bash
# xaa-keys: the public keys other parties register, into .lab/xaa/keys/. Every
# key is stable across rebuilds (.lab/keys); private halves never leave the lab.
#
#   sv-upstream-client.jwks.json  S&V's client at an upstream IdP (e.g. Gluu):
#                                 the broker's key (Keycloak, PS256) and the
#                                 egress's key (idtoken-exchange, xaa-relay)
#   sv-ras-client.jwks.json       S&V's client at Ledgerline's AS
#                                 (sterling-vance-kagent)
#   sv-idp.jwks.json              S&V's Keycloak token-signing keys, issuer
#                                 https://idp.sterling.lab/realms/sterling-vance,
#                                 for an AS trusting its ID-JAGs (that issuer is
#                                 not reachable from the internet)
#   ledgerline-sso-client.jwks.json  Ledgerline's SSO client ("ledgerline") at
#                                 S&V's IdPs
. "$(dirname "$0")/lib.sh"
. "$LAB_ROOT/scripts/idp.sh"
need_cluster
out="$LAB_STATE/xaa/keys"; mkdir -p "$out"
for k in sv-xaa-client sv-egress-client; do realm_signing_key "$k"; done
lp=$(free_port); port_forward sv-identity keycloak "$lp" 80
certs=$(curl -sf "http://127.0.0.1:$lp/realms/sterling-vance/protocol/openid-connect/certs") || die "no JWKS from S&V's Keycloak"
echo "$certs" | jq --argjson egress "$(jwks_of "$LAB_STATE/keys/sv-egress-client.crt")" \
  '{keys: ([.keys[] | select(.use == "sig" and .alg == "PS256")] + $egress.keys)}' >"$out/sv-upstream-client.jwks.json"
xaa_client_jwks >"$out/sv-ras-client.jwks.json"
echo "$certs" | jq '{keys: [.keys[] | select(.use == "sig" and .alg == "RS256")]}' >"$out/sv-idp.jwks.json"
lp=$(free_port); port_forward ledgerline-identity keycloak "$lp" 80
curl -sf "http://127.0.0.1:$lp/realms/ledgerline/protocol/openid-connect/certs" \
  | jq '{keys: [.keys[] | select(.use == "sig" and .alg == "PS256")]}' >"$out/ledgerline-sso-client.jwks.json" \
  || die "no JWKS from Ledgerline's Keycloak"
for f in "$out"/*.json; do printf '  %-34s %s\n' "$(basename "$f")" "$(jq -r '[.keys[].kid] | join(", ")' "$f")"; done
ok "public keys: $out"
