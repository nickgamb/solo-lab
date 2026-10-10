# solo-lab — Solo.io AI platform lab on kind.  `make help` for targets.
SHELL := /bin/bash
.DEFAULT_GOAL := help
LAYERS := $(sort $(wildcard platform/[0-9][0-9]-*/install.sh))

## ---- one-time machine setup (sudo, run yourself) ----------------------------
machine-setup: ## one-time, sudo: *.lab resolves to the lab, trust the lab CA (macOS, Linux, WSL2)
	@./scripts/machine-setup.sh

## ---- lifecycle ---------------------------------------------------------------
up: ## preflight, then create the cluster and install the whole platform
	@./scripts/preflight.sh && LAB_PREFLIGHT_DONE=1 $(MAKE) --no-print-directory cluster && $(MAKE) --no-print-directory platform
cluster: ## kind cluster + registry caches + cloud-provider-kind + lab DNS
	@{ [ "$${LAB_PREFLIGHT_DONE:-}" = 1 ] || ./scripts/preflight.sh; } && ./scripts/dns.sh && ./scripts/ca.sh && LAB_PREFLIGHT_DONE=1 ./scripts/cluster-up.sh
platform: ## install every platform layer in order (idempotent)
	@for l in $(LAYERS); do bash $$l || exit 1; done
layer-%: ## install one layer, e.g. make layer-40 (agentgateway)
	@f="$(wildcard platform/$*-*/install.sh)"; [ -n "$$f" ] || { echo "no layer $*; layers: $(patsubst platform/%/install.sh,%,$(LAYERS))" >&2; exit 2; }; bash $$f
down: ## delete the cluster (keeps image caches and the CA)
	@./scripts/cluster-down.sh
nuke: ## delete the cluster AND every lab container and volume (caches, registry with built images, lab DNS)
	@./scripts/cluster-down.sh --all

## ---- demos (all installed by `make up`; cards in docs/cards) ---------------
verify: ## rewind the demos (make reset), every story's checks, identity continuity, the Observatory's reach
	@./scripts/reset.sh && ./demos/bob/verify.sh && ./demos/bob-to-alice/verify.sh && ./platform/47-continuity/verify.sh && ./platform/90-observatory/verify.sh
bob-verify: ## story 1 checks only (delegation, per-tool policy, Cross App Access)
	@./demos/bob/verify.sh
alice-verify: ## story 2 checks only, from a first run (make reset first)
	@./scripts/reset.sh && ./demos/bob-to-alice/verify.sh
continuity-verify: ## identity continuity checks only (failover, kill switch, live rules)
	@./platform/47-continuity/verify.sh
observatory-verify: ## what the Observatory's admins may and may not change (RBAC, admission)
	@./platform/90-observatory/verify.sh
tour: ## drive every story end to end, paced, to watch live in the Observatory
	@./scripts/tour.sh
reset: ## rewind every demo to a first run (grants, terms, agent key, follow-ups)
	@./scripts/reset.sh

## ---- day to day -------------------------------------------------------------
llm: ## switch the LLM backend: make llm LLM_PROVIDER=ollama|anthropic|openai
	@./scripts/llm.sh
xaa-logs: ## Cross App Access trail (both token requests, claims, checks), tokens redacted: make xaa-logs SINCE=2h
	@./scripts/xaa-logs.sh --since $(or $(SINCE),1h)
routes: ## where sign-ins go now: each routing rule's IdP, and where sample sign-ins land
	@./scripts/routes.sh
xaa-keys: ## public keys other parties register: S&V clients (private_key_jwt), S&V IdP, Ledgerline SSO client
	@./scripts/xaa-keys.sh
totp: ## one-time code for signing in at S&V's own Keycloak as bob (or: make totp EMPLOYEE=carol)
	@bash -c '. scripts/lib.sh && totp_code $(or $(EMPLOYEE),bob)'
status: ## what's running, where, and the URLs
	@./scripts/status.sh
preflight: ## check tools and Docker resources
	@./scripts/preflight.sh

help:
	@awk 'BEGIN{FS=":.*## "} /^## ----/{printf "\n\033[1m%s\033[0m\n", substr($$0,9)} /^[a-zA-Z%_-]+:.*## /{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: machine-setup up cluster platform down nuke verify bob-verify alice-verify continuity-verify observatory-verify tour reset llm xaa-logs xaa-keys routes totp status preflight help
