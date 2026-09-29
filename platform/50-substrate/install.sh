#!/usr/bin/env bash
# Agent Substrate: agents as snapshot-backed actors in gVisor sandboxes on a
# small pool of pre-warmed workers (thousands of agents on tens of pods).
#   substrate 0.0.9, the pairing kagent 0.10.x vendors (kagent-enterprise 1.0
#   pairs with 0.2.x; not wired here, docs/ENTERPRISE.md)
# ate-system is in the ambient mesh like every platform namespace (mesh.yaml).
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster

lab_image "kagent-dev/substrate/ateom-gvisor:$SUBSTRATE_LAB_TAG" tools/substrate-mesh/build.sh
step "Agent Substrate $SUBSTRATE_VERSION"
# The release's CRD chart with the WorkerPool CRD from tools/substrate-mesh
# (pod identity), so helm owns all three.
CRDS="$LAB_STATE/cache/substrate-crds-$SUBSTRATE_VERSION"
if [ ! -d "$CRDS" ]; then
  rm -rf "$CRDS.tmp"   # an earlier pull that didn't finish
  H pull "$SUBSTRATE_CRDS_CHART" --version "$SUBSTRATE_VERSION" --untar --untardir "$CRDS.tmp" >"$LAB_STATE/helm-substrate-crds-pull.log" 2>&1 \
    || die "pulling $SUBSTRATE_CRDS_CHART $SUBSTRATE_VERSION failed (log: .lab/helm-substrate-crds-pull.log)"
  mv "$CRDS.tmp/substrate-crds" "$CRDS"; rm -rf "$CRDS.tmp"
fi
cp "$LAB_ROOT/tools/substrate-mesh/crds/ate.dev_workerpools.yaml" "$CRDS/templates/"
helm_up substrate-crds "$CRDS" "" ate-system
values_for "$D" values oss
HELM_TIMEOUT=15m helm_up substrate "$SUBSTRATE_CHART" "$SUBSTRATE_VERSION" ate-system ${VALS[@]+"${VALS[@]}"}

# Gate on the failure that otherwise shows up much later: a valkey cluster
# that never reaches cluster_state:ok leaves ate-api restarting in a loop.
wait_for "valkey cluster_state:ok" 60 5 sh -c "[ \"\$(kubectl --context $KCTX exec -n ate-system valkey-cluster-0 -- redis-cli -p 6379 cluster info 2>/dev/null | tr -d '\r' | awk -F: '/^cluster_state:/{print \$2}')\" = ok ]"
ok "valkey cluster_state:ok"
apply_tmpl "$D/mesh.yaml"
ok "STRICT; router <- kagent controller; ate-api <- kagent controller"
K get pods -n ate-system --no-headers | awk '{print "    "$1" "$3}'
