#!/usr/bin/env bash
# Story 2 (Bob -> Alice): Alice's own IdP, authorization server and portal;
# Meridian (her brokerage) enforcing her terms; and the U4A adapter on the
# Sterling & Vance side. Story 1 is untouched: bob-assistant gains one tool.
# Layered on story 1 (platform/95-demos runs both). Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
export U4A_TAG=${U4A_TAG:-u4a-ea9d86f}      # uma4agents commit the images are built from
U4A_SRC=${U4A_SRC:-$HOME/Documents/GitHub/uma4agents}

step "u4a images ($U4A_TAG)"
img() { docker image inspect "localhost:$LAB_REGISTRY_PORT/u4a/$1:$U4A_TAG" >/dev/null 2>&1 \
  || { [ -d "$U4A_SRC" ] || die "uma4agents not found at $U4A_SRC (set U4A_SRC)";
       docker build -q -t "localhost:$LAB_REGISTRY_PORT/u4a/$1:$U4A_TAG" -f "$2" "$3" >/dev/null; }
  docker push -q "localhost:$LAB_REGISTRY_PORT/u4a/$1:$U4A_TAG" >/dev/null; }
img uma-as          "$U4A_SRC/services/uma-as/Dockerfile"        "$U4A_SRC"
img uma-pep         "$U4A_SRC/services/uma-pep/Dockerfile"       "$U4A_SRC"
img alice-vault-mcp "$U4A_SRC/mcp/alice-vault/Dockerfile"        "$U4A_SRC"
img portal          "$U4A_SRC/services/alice-portal/Dockerfile"  "$U4A_SRC/services/alice-portal"
img agent-adapter   "$D/adapter/Dockerfile"                      "$U4A_SRC"
ok "uma-as uma-pep alice-vault-mcp portal agent-adapter"

step "CloudNativePG operator (Alice's database)"
K create namespace cnpg-system --dry-run=client -o yaml | K apply -f - >/dev/null
K label namespace cnpg-system istio.io/dataplane-mode=ambient lab.solo.io/party=platform --overwrite >/dev/null
helm_up cnpg cloudnative-pg "$CNPG_VERSION" cnpg-system --repo https://cloudnative-pg.github.io/charts

step "Alice's IdP (alice-identity)"
K create secret generic kc-secrets -n alice-identity \
  --from-literal=KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  --from-literal=KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret ALICE_KC_ADMIN_PASSWORD)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
deploy_keycloak alice-identity "$ALICE_DOMAIN" https-alice "$D/realm-alice.json"
apply_tmpl "$D/alice-identity.yaml"
ok "https://idp.$ALICE_DOMAIN/realms/alice"

step "Signing keys and per-party secrets"
ed25519() {  # ed25519 <name>: generated once, stable across rebuilds
  local f="$LAB_STATE/keys/$1.pem"; mkdir -p "$LAB_STATE/keys"
  [ -s "$f" ] || { openssl genpkey -algorithm ed25519 -out "$f" 2>/dev/null; chmod 600 "$f"; }
  echo "$f"; }
K create secret generic uma-as-signing-key -n alice --from-file=uma-as-ed25519.pem="$(ed25519 uma-as)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
K create secret generic uma-pep-signing-key -n meridian --from-file=uma-pep-ed25519.pem="$(ed25519 uma-pep)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# The RS credential Alice's AS issued Meridian. Each party holds its own copy:
# a shared Secret would model them as one party.
RS=$(lab_secret MERIDIAN_RS_CLIENT_SECRET)
K create secret generic uma-as-config -n alice --from-literal=rs-client-secret="$RS" --dry-run=client -o yaml | K apply -f - >/dev/null
K create secret generic portal-config -n alice --from-literal=session-secret="$(lab_secret ALICE_PORTAL_SESSION_SECRET)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
K create secret generic meridian-config -n meridian --from-literal=rs-client-secret="$RS" \
  --from-literal=rs-secrets="{\"alice\": \"$RS\"}" --dry-run=client -o yaml | K apply -f - >/dev/null
ok "uma-as, uma-pep keys; RS credential per party"

step "Alice (alice), Meridian (meridian), S&V adapter (sv-u4a)"
apply_tmpl "$D/alice.yaml" "$D/meridian.yaml" "$D/sv-u4a.yaml"
wait_for "Alice's database" 60 5 K wait cluster/uma-as-db -n alice --for=condition=Ready --timeout=2s
rollout alice deploy/uma-as deploy/portal
rollout meridian deploy/uma-pep deploy/alice-vault deploy/meridian
rollout sv-u4a deploy/u4a-adapter
ok "https://portal.$ALICE_DOMAIN  https://as.$ALICE_DOMAIN  https://gateway.$MERIDIAN_DOMAIN"

step "Bob's agent: one more tool (kustomize overlay on story 1's agent)"
K apply -k "$D/agent" >/dev/null
wait_for "bob-assistant Ready" 60 5 K wait agent/bob-assistant -n sv-agents --for=condition=Ready --timeout=2s
ok "Alice: https://portal.$ALICE_DOMAIN (alice / alice-demo). Bob: https://kagent.$SV_DOMAIN"
