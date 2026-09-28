# solo-lab — Solo.io AI platform lab on kind.  `make help` for targets.
SHELL := /bin/bash
.DEFAULT_GOAL := help
LAYERS := $(sort $(wildcard platform/[0-9][0-9]-*/install.sh))

## ---- one-time machine setup (sudo, run yourself) ----------------------------
machine-setup: ## one-time, one sudo: *.lab resolver + trust the lab CA
	@./scripts/dns.sh && ./scripts/ca.sh
	sudo sh -c 'mkdir -p /etc/resolver && printf "nameserver 127.0.0.1\nport 15353\n" > /etc/resolver/lab && security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain '"$$HOME"'/.solo-lab/ca/ca.crt'
	@dscacheutil -q host -a name portal.alice.lab | grep -q 127.0.0.1 && echo "  ✓ *.lab resolves to 127.0.0.1" || echo "  ! resolver not answering yet"
	@security verify-cert -c $$HOME/.solo-lab/ca/ca.crt >/dev/null 2>&1 && echo "  ✓ lab CA trusted (name-constrained to .lab/.svc/.cluster.local)"

## ---- lifecycle ---------------------------------------------------------------
up: cluster platform ## create the cluster and install the whole platform
cluster: ## kind cluster + registry caches + cloud-provider-kind + lab DNS
	@./scripts/dns.sh && ./scripts/ca.sh && ./scripts/cluster-up.sh
platform: ## install every platform layer in order (idempotent)
	@for l in $(LAYERS); do bash $$l || exit 1; done
layer-%: ## install one layer, e.g. make layer-40 (agentgateway)
	@bash $(wildcard platform/$*-*/install.sh)
down: ## delete the cluster (keeps image caches and the CA)
	@./scripts/cluster-down.sh
nuke: ## delete the cluster AND the image caches
	@./scripts/cluster-down.sh --all

## ---- demos ------------------------------------------------------------------
bob: ## story 1: Bob's agent, delegation, Cross App Access (ID-JAG)
	@./demos/bob/install.sh
bob-verify: ## story 1: enforcement checks from real mesh identities
	@./demos/bob/verify.sh

## ---- day to day -------------------------------------------------------------
llm: ## switch the LLM backend: make llm LLM_PROVIDER=ollama|anthropic|openai
	@./scripts/llm.sh
status: ## what's running, where, and the URLs
	@./scripts/status.sh
preflight: ## check tools and Docker resources
	@./scripts/preflight.sh

help:
	@awk 'BEGIN{FS=":.*## "} /^## ----/{printf "\n\033[1m%s\033[0m\n", substr($$0,9)} /^[a-zA-Z%_-]+:.*## /{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: machine-setup up cluster platform down nuke bob bob-verify llm status preflight help
