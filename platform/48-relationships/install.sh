#!/usr/bin/env bash
# relationships — S&V's entitlements in OpenFGA: who advises which clients,
# which agents act for whom, who may call each MCP server. OpenFGA runs
# highly available on Postgres in sv-authz; the continuity controller (layer
# 47) keeps its store matching the RelationshipModel and Relationship
# resources; the gateway's relationship guard (demos/bob) asks it on every
# tool call.
. "$(dirname "$0")/../../scripts/lib.sh"
D="$(cd "$(dirname "$0")" && pwd)"
need_cluster

step "CloudNativePG operator (OpenFGA's database)"
install_cnpg

step "OpenFGA $OPENFGA_CHART_VERSION (sv-authz)"
K apply -f "$D/openfga.yaml" >/dev/null
# the pre-shared key OpenFGA takes: the one the controller holds (layer 47)
# and the relationship guard is given (demos/bob)
secret_apply sv-authz openfga-authn keys="$(lab_secret SV_OPENFGA_KEY)"
wait_for "OpenFGA's database" 180 5 K wait cluster/openfga-db -n sv-authz --for=condition=Ready --timeout=2s
helm_up openfga openfga "$OPENFGA_CHART_VERSION" sv-authz --repo https://openfga.github.io/helm-charts -f "$D/openfga-values.yaml"
rollout sv-authz deploy/openfga
deny_internet sv-authz
ok "OpenFGA: 3 replicas, Postgres (3 instances), callers: the relationship guard and the continuity controller"

step "S&V's relationship model"
K apply -f "$D/model.yaml" >/dev/null
synced() { [ "$(K get relm sterling-vance -n sv-identity -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}')" = True ]; }
wait_for "RelationshipModel sterling-vance Synced" 120 3 synced
ok "store $(K get relm sterling-vance -n sv-identity -o jsonpath='{.status.storeID}'), model $(K get relm sterling-vance -n sv-identity -o jsonpath='{.status.modelID}')"
