#!/usr/bin/env bash
# What's running, what isn't, who signs people in, and where everything is.
. "$(dirname "$0")/lib.sh"
need_cluster

step "Cluster ($KCTX)"
K get nodes --no-headers | awk '{printf "  %-28s %s\n", $1, $2}'
bad=$(K get pods -A --no-headers 2>/dev/null | awk '$4 != "Running" && $4 != "Completed" {print "  " $1 "/" $2 "  " $4}')
[ -z "$bad" ] && ok "every pod Running or Completed" || { warn "pods not running:"; echo "$bad"; }

step "Sign-in (IdentityContinuity sv-identity/sterling-vance)"
if K get idc sterling-vance -n sv-identity >/dev/null 2>&1; then
  K get idc sterling-vance -n sv-identity -o json | jq -r '"  active: \(.status.active // "none") since \(.status.activeSince // "-")",
    (.status.tiers[]? | "  \(.name): \(if .configured then (if .healthy then "healthy" else "unhealthy" end) else "not configured" end)\(if .partitioned then " (partitioned)" else "" end)  \(.message // "")")'
else
  warn "not installed (make layer-47)"
fi

step "URLs"
row() { printf '  %-36s %-22s %s\n' "$@"; }
row "https://observatory.$OPS_DOMAIN" "Observatory" "ops / ops-demo (realm ops)"
row "https://grafana.$OPS_DOMAIN" "Grafana" "admin / solo-lab"
row "https://kiali.$OPS_DOMAIN" "Kiali" "no sign-in (anonymous)"
row "https://kagent.$SV_DOMAIN" "kagent" "bob / bob-demo, or Bob's upstream IdP account"
row "https://registry.$SV_DOMAIN" "agentregistry" "S&V sign-in (bob or ops)"
row "https://idp.$SV_DOMAIN" "S&V Keycloak" "admin password: SV_KC_ADMIN_PASSWORD in .lab/secrets.env"
row "https://portal.$ALICE_DOMAIN" "Alice's portal" "alice / alice-demo"
