#!/usr/bin/env bash
# Agentshield: the platform team's view of lateral movement. It builds a graph
# of the cluster (workloads, identities, mesh and gateway policy, exposures)
# from the Kubernetes API, adds the requests the waypoints see (their access
# logs, through the collector), flags new paths with evidence, and proposes
# fixes; its investigations and rule drafts go to a model through the AI
# gateway. Console at https://agentshield.<ops>, behind the admins' sign-in.
# AGENTSHIELD=off in .env removes it. Idempotent.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
NS=agentshield-system
need_cluster

if ! agentshield_on; then
  step "Agentshield: off (AGENTSHIELD=off)"
  H uninstall agentshield -n "$NS" --wait >/dev/null 2>&1 || true
  K delete -f <(render "$D/agentshield.yaml") --ignore-not-found >/dev/null 2>&1 || true
  K delete secret agentshield-sso -n "$NS" --ignore-not-found >/dev/null
  ok "removed (its access logs and model route stay off until layers 10, 20 and 40 run again)"
  exit 0
fi

step "agentshield image ($AGENTSHIELD_COMMIT, tools/agentshield)"
IMG=$(agentshield_image)
lab_image "$IMG" tools/agentshield/build.sh
SRC=$(agentshield_src)   # its chart, from the same patched source
export AGENTSHIELD_TAG=${IMG##*:}
ok "localhost:$LAB_REGISTRY_PORT/$IMG"

step "Agentshield's database (CloudNativePG)"
install_cnpg
K apply -f "$D/../00-foundation/namespaces.yaml" >/dev/null   # agentshield-system, on a lab from before it
secret_apply "$NS" agentshield-sso client-secret="$(lab_secret OPS_AGENTSHIELD_CLIENT_SECRET)"
apply_tmpl "$D/agentshield.yaml"
wait_for "Agentshield's database" 180 5 K wait cluster/agentshield-db -n "$NS" --for=condition=Ready --timeout=2s
ok "agentshield-db: callers Agentshield and the operator"

step "Agentshield (its chart)"
values_for "$D" values oss
helm_up agentshield "$SRC/deploy/kubernetes" "" "$NS" ${VALS[@]+"${VALS[@]}"}
deny_internet "$NS"
ok "https://agentshield.$OPS_DOMAIN  (ops / $(password_hint ops)); its model: the AI gateway's security route"
