#!/usr/bin/env bash
# Agentshield: its graph is published (Enterprise agentgateway included), a
# request a waypoint serves reaches it through the collector, and it is
# reachable only as intended: the console through the edge's sign-in, the
# API from nothing else in the mesh, its model route by nobody else.
. "$(dirname "$0")/../../scripts/lib.sh"
need_cluster
agentshield_on || { ok "Agentshield is off (AGENTSHIELD=off): nothing to check"; exit 0; }
NS=agentshield-system
pass=0 fail=0
res() { if [ "$1" = ok ]; then ok "$2"; pass=$((pass+1)); else warn "$2"; echo "      got: ${3:0:300}"; fail=$((fail+1)); fi; }
expect() { echo "$3" | grep -qE "$1" && res ok "$2" || res no "$2" "$3"; }

# its API, through a port-forward (as an admin with kubectl would)
port=$(free_port); kubectl --context "$KCTX" port-forward -n "$NS" deploy/agentshield "$port:8088" >/dev/null 2>&1 & pf=$!
on_exit "kill $pf 2>/dev/null"
wait_for "Agentshield's API" 30 1 curl -sf -o /dev/null "http://127.0.0.1:$port/readyz"
api() { curl -s "http://127.0.0.1:$port/api/v1/$1"; }

step "The graph"
ov=$(api overview)
expect '^[1-9]' "resources in the graph" "$(jq -r '.counts.resources' <<<"$ov")"
expect '^agentgateway istio kubernetes$' "the Kubernetes, Istio and agentgateway contributions are ready" \
  "$(jq -r '[.manifest.contributions[] | select(.ready) | .pluginId] | map(select(. == "kubernetes" or . == "istio" or . == "agentgateway")) | unique | join(" ")' <<<"$ov")"
gw=$(api graph | jq -r '[.. | strings | select(. == "agentgateway-ingress")] | length')
expect '^[1-9]' "the lab's agentgateway Gateways ($AGW_EDITION) are in it" "$gw"

step "A waypoint's requests reach it"
before=$(api overview | jq -r '.counts.telemetry')
IMG=$(lab_build lab/toolbox "$LAB_ROOT/tools/toolbox")
K -n meridian create deployment agentshield-verify --image="$IMG" -- \
  sh -c 'while true; do curl -s -o /dev/null http://uma-pep.meridian:9002/owner-resources; sleep 3; done' >/dev/null 2>&1
on_exit "K -n meridian delete deployment agentshield-verify --wait=false >/dev/null 2>&1"
more() { [ "$(api overview | jq -r '.counts.telemetry')" -gt "$before" ]; }
if wait_for "a refused request through meridian's waypoint, in Agentshield" 180 5 more; then
  res ok "the waypoint's access log reached it through the collector (refused, so named from the waypoint's route)"
else
  res no "the waypoint's access log reached it" "telemetry $before -> $(api overview | jq -r '.counts.telemetry')"
fi

step "Who may reach it"
expect '^302 https://idp\.'"${OPS_DOMAIN//./\\.}"'/realms/ops/' "the console signs admins in first (ops realm)" \
  "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' --cacert "$LAB_TRUST" "https://agentshield.$OPS_DOMAIN/")"
probe_pod meridian waypoint >/dev/null
expect '^000' "another workload can't reach its API" \
  "$(K exec -n meridian probe-waypoint -- curl -s -m 5 -o /dev/null -w '%{http_code}' "http://agentshield.$NS:8088/api/v1/overview" 2>/dev/null || true)"
expect '^403' "another workload can't use its model route" \
  "$(K exec -n meridian probe-waypoint -- curl -s -m 20 -o /dev/null -w '%{http_code}' -H 'content-type: application/json' \
      -d '{"model":"x","input":"hi"}' http://ai-gateway.agentgateway-system/security/v1/responses || true)"
expect '^$' "no model call of its own has failed for good" \
  "$(K logs -n "$NS" deploy/agentshield 2>&1 | grep -i 'failed permanently' | tail -1)"

[ $fail -eq 0 ] && ok "Agentshield: $pass/$((pass+fail)) checks passed" || die "Agentshield: $fail of $((pass+fail)) checks failed"
