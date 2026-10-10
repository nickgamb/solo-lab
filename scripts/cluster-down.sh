#!/usr/bin/env bash
# Delete the cluster. Registry caches are kept (they are what make the next
# `make up` fast); `make nuke` removes them too.
. "$(dirname "$0")/lib.sh"

step "Deleting kind cluster $LAB_NAME"
kind delete cluster --name "$LAB_NAME" 2>/dev/null && ok "deleted" || ok "not present"
# cloud-provider-kind holds the deleted cluster's credentials: the next
# cluster gets a fresh one (make up)
docker rm -f lab-cloud-provider-kind >/dev/null 2>&1 || true

if [ "${1:-}" = --all ]; then
  step "Removing registry caches, the local registry (and every image built into it), lab DNS, cloud-provider-kind"
  for c in $(docker ps -aq -f name='^lab-'); do docker rm -f "$c" >/dev/null; done
  for v in $(docker volume ls -q -f name='^lab-'); do docker volume rm "$v" >/dev/null; done
  ok "removed"
fi
