#!/usr/bin/env bash
# Delete the cluster. Registry caches are kept (they are what make the next
# `make up` fast); `make nuke` removes them too.
. "$(dirname "$0")/lib.sh"

step "Deleting kind cluster $LAB_NAME"
kind delete cluster --name "$LAB_NAME" 2>/dev/null && ok "deleted" || ok "not present"

if [ "${1:-}" = --all ]; then
  step "Removing registry caches, local registry, cloud-provider-kind"
  for c in $(docker ps -aq -f name='^lab-'); do docker rm -f "$c" >/dev/null; done
  for v in $(docker volume ls -q -f name='^lab-'); do docker volume rm "$v" >/dev/null; done
  ok "removed"
fi
