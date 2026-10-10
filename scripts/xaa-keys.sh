#!/usr/bin/env bash
# xaa-keys: the public keys other parties register, into .lab/xaa/keys/. Every
# key is stable across rebuilds (.lab/keys); private halves never leave the lab.
# Runs before make up too, so the keys can be registered first.
#
#   sv-upstream-client.jwks.json  S&V's client at an upstream IdP (e.g. Gluu):
#                                 the broker's key (Keycloak, PS256)
#   sv-ras-client.jwks.json       S&V's client at Ledgerline's AS
#                                 (sterling-vance-kagent)
#   sv-idp.jwks.json              S&V's broker's token-signing keys, issuer
#                                 https://idp.sterling.lab/realms/sterling-vance,
#                                 for an AS trusting its ID-JAGs (that issuer is
#                                 not reachable from the internet)
#   ledgerline-sso-client.jwks.json  Ledgerline's SSO client ("ledgerline") at
#                                 S&V's broker
. "$(dirname "$0")/lib.sh"
. "$LAB_ROOT/scripts/idp.sh"
# no cluster needed: the keys are the lab's own (.lab/keys), made here if this
# runs before make up, so they can be registered before the lab is built
out="$LAB_STATE/xaa/keys"; mkdir -p "$out"
sv_upstream_client_jwks >"$out/sv-upstream-client.jwks.json"
xaa_client_jwks >"$out/sv-ras-client.jwks.json"
kc_jwks sterling-vance RS256 >"$out/sv-idp.jwks.json"
rm -f "$out/sv-workforce.jwks.json"   # earlier labs: S&V's own Keycloak vouched too
kc_jwks ledgerline-sso-client PS256 >"$out/ledgerline-sso-client.jwks.json"
for f in "$out"/*.json; do printf '  %-34s %s\n' "$(basename "$f")" "$(jq -r '[.keys[].kid] | join(", ")' "$f")"; done
ok "public keys: $out"
