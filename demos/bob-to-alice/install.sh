#!/usr/bin/env bash
# Story 2 (Bob -> Alice): Alice's own IdP, authorization server and portal;
# Meridian (her brokerage) enforcing her terms; and the U4A adapter on the
# Sterling & Vance side. Story 1 is untouched: bob-assistant gains one tool.
# Layered on story 1 (platform/95-demos runs both). Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster
export U4A_TAG=${U4A_TAG:-u4a-ea9d86f}      # uma4agents commit the images are built from
# ...in full: the commit hash is the checksum of everything exported below.
# Another U4A_TAG takes its own U4A_SHA (or is resolved in the checkout).
[ "$U4A_TAG" = u4a-ea9d86f ] && U4A_SHA=${U4A_SHA:-ea9d86fc924b7605581ad90a76af500d0be6fd66}
# The images are built from exactly that commit, whatever state a checkout is
# in: U4A_SRC, else a checkout beside this repo, else a clone under .lab/,
# exported at the commit into .lab/cache.
U4A_SRC=${U4A_SRC:-$( [ -d "$LAB_ROOT/../uma4agents" ] && cd "$LAB_ROOT/../uma4agents" && pwd || echo "$LAB_STATE/uma4agents")}
if [ ! -d "$U4A_SRC" ]; then
  rm -rf "$U4A_SRC.tmp"
  git clone -q https://github.com/nickgamb/uma4agents "$U4A_SRC.tmp" || { rm -rf "$U4A_SRC.tmp"; die "cloning uma4agents failed"; }
  mv "$U4A_SRC.tmp" "$U4A_SRC"
fi
U4A_COMMIT=${U4A_TAG#u4a-}
U4A_REV=${U4A_SHA:-$U4A_COMMIT}
if ! git -C "$U4A_SRC" cat-file -e "$U4A_REV^{commit}" 2>/dev/null; then
  git -C "$U4A_SRC" fetch -q origin || die "fetching uma4agents into $U4A_SRC failed"
fi
got=$(git -C "$U4A_SRC" rev-parse --verify -q "$U4A_REV^{commit}") || die "uma4agents commit $U4A_REV not found in $U4A_SRC"
case "$got" in "$U4A_COMMIT"*) ;; *) die "U4A_TAG $U4A_TAG and uma4agents commit $got disagree" ;; esac
[ -z "${U4A_SHA:-}" ] || [ "$got" = "$U4A_SHA" ] || die "uma4agents $U4A_TAG is $got in $U4A_SRC, not $U4A_SHA"
U4A_CTX="$LAB_STATE/cache/uma4agents-$U4A_COMMIT"
if [ ! -d "$U4A_CTX" ]; then
  rm -rf "$U4A_CTX.tmp"; mkdir -p "$U4A_CTX.tmp"
  git -C "$U4A_SRC" archive "$got" | tar -x -C "$U4A_CTX.tmp" || { rm -rf "$U4A_CTX.tmp"; die "exporting uma4agents $got failed"; }
  mv "$U4A_CTX.tmp" "$U4A_CTX"
fi
for f in services/uma-as/Dockerfile services/uma-pep/Dockerfile mcp/alice-vault/Dockerfile services/alice-portal/Dockerfile; do
  [ -f "$U4A_CTX/$f" ] || die "uma4agents $U4A_COMMIT has no $f ($U4A_CTX)"
done

step "u4a images ($U4A_TAG)"
img() {  # img <name> <dockerfile> <context> [tag]
  local tag=${4:-$U4A_TAG}
  in_registry "u4a/$1:$tag" || { docker build -q -t "localhost:$LAB_REGISTRY_PORT/u4a/$1:$tag" -f "$2" "$3" >/dev/null \
    && docker push -q "localhost:$LAB_REGISTRY_PORT/u4a/$1:$tag" >/dev/null; } || die "building u4a/$1 failed"
}
# the adapter's Dockerfile is this repo's: its tag carries that too
export U4A_ADAPTER_TAG; U4A_ADAPTER_TAG="$U4A_TAG-$(sha1 < "$D/adapter/Dockerfile" | cut -c1-8)"
img uma-as          "$U4A_CTX/services/uma-as/Dockerfile"        "$U4A_CTX"
img uma-pep         "$U4A_CTX/services/uma-pep/Dockerfile"       "$U4A_CTX"
img alice-vault-mcp "$U4A_CTX/mcp/alice-vault/Dockerfile"        "$U4A_CTX"
img portal          "$U4A_CTX/services/alice-portal/Dockerfile"  "$U4A_CTX/services/alice-portal"
img agent-adapter   "$D/adapter/Dockerfile"                      "$U4A_CTX" "$U4A_ADAPTER_TAG"
ok "uma-as uma-pep alice-vault-mcp portal agent-adapter"

step "CloudNativePG operator (Alice's database)"
K create namespace cnpg-system --dry-run=client -o yaml | K apply -f - >/dev/null
K label namespace cnpg-system istio.io/dataplane-mode=ambient lab.solo.io/party=platform --overwrite >/dev/null
# mutual TLS mesh-wide (layer 80), but the API server calls the operator's
# admission webhook from outside the mesh
K apply -f - >/dev/null <<'YAML'
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata: {name: webhook, namespace: cnpg-system}
spec:
  selector: {matchLabels: {app.kubernetes.io/name: cloudnative-pg}}
  mtls: {mode: STRICT}
  portLevelMtls: {"9443": {mode: PERMISSIVE}}
YAML
helm_up cnpg cloudnative-pg "$CNPG_VERSION" cnpg-system --repo https://cloudnative-pg.github.io/charts
deny_internet cnpg-system

step "Alice's IdP (alice-identity)"
secret_apply alice-identity kc-secrets \
  KC_BOOTSTRAP_ADMIN_USERNAME=admin KC_BOOTSTRAP_ADMIN_PASSWORD="$(lab_secret ALICE_KC_ADMIN_PASSWORD)"
deploy_keycloak alice-identity "$ALICE_DOMAIN" https-alice "$D/realm-alice.json"
apply_tmpl "$D/alice-identity.yaml"
ok "https://idp.$ALICE_DOMAIN/realms/alice"

step "Signing keys and per-party secrets"
ed25519() {  # ed25519 <name>: generated once, stable across rebuilds
  local f="$LAB_STATE/keys/$1.pem"; mkdir -p "$LAB_STATE/keys"
  [ -s "$f" ] || { openssl genpkey -algorithm ed25519 -out "$f" 2>/dev/null && chmod 600 "$f"; } \
    || { rm -f "$f"; die "openssl can't make an ed25519 key ($(openssl version)); OpenSSL 3 is needed (make preflight)"; }
  echo "$f"; }
K create secret generic uma-as-signing-key -n alice --from-file=uma-as-ed25519.pem="$(ed25519 uma-as)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
K create secret generic uma-pep-signing-key -n meridian --from-file=uma-pep-ed25519.pem="$(ed25519 uma-pep)" \
  --dry-run=client -o yaml | K apply -f - >/dev/null
# The RS credential Alice's AS issued Meridian. Each party holds its own copy:
# a shared Secret would model them as one party.
RS=$(lab_secret MERIDIAN_RS_CLIENT_SECRET)
secret_apply alice uma-as-config rs-client-secret="$RS"
secret_apply alice portal-config session-secret="$(lab_secret ALICE_PORTAL_SESSION_SECRET)"
secret_apply meridian meridian-config rs-client-secret="$RS" rs-secrets="{\"alice\": \"$RS\"}"
ok "uma-as, uma-pep keys; RS credential per party"

step "Alice (alice), Meridian (meridian), S&V adapter (sv-u4a)"
# earlier labs: Istio's own waypoint for Alice, and its L7 rules as Istio policy
if [ "$(K get gateway waypoint -n alice -o jsonpath='{.spec.gatewayClassName}' 2>/dev/null)" = istio-waypoint ]; then
  K delete gateway waypoint -n alice --wait >/dev/null
fi
K delete authorizationpolicy uma-as-grant-surface -n alice --ignore-not-found >/dev/null
apply_tmpl "$D/alice.yaml" "$D/meridian.yaml" "$D/sv-u4a.yaml"
deny_internet alice alice-identity meridian sv-u4a   # every party reaches the others through the edge
wait_for "Alice's database" 60 5 K wait cluster/uma-as-db -n alice --for=condition=Ready --timeout=2s
rollout alice deploy/uma-as deploy/portal
rollout meridian deploy/uma-pep deploy/alice-vault deploy/meridian
rollout sv-u4a deploy/u4a-adapter
ok "https://portal.$ALICE_DOMAIN  https://as.$ALICE_DOMAIN  https://gateway.$MERIDIAN_DOMAIN"

step "Bob's agent: one more tool (kustomize overlay on story 1's agent)"
apply_kustomize "$D/agent"
wait_for "bob-assistant Ready" 60 5 K wait sandboxagent/bob-assistant -n sv-agents --for=condition=Ready --timeout=2s
ok "Alice: https://portal.$ALICE_DOMAIN (alice / alice-demo). Bob: https://kagent.$SV_DOMAIN"
