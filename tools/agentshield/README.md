# Agentshield patches (upstream PR candidates)

Three patches against [agentshield-dev/agentshield](https://github.com/agentshield-dev/agentshield)
at `AGENTSHIELD_COMMIT` (`config/oss.env`). Agentshield publishes no image or
chart yet: layer 92 builds both from that commit with these applied.

## 0001: agentgateway Gateways by any GatewayClass controller

**Why.** The agentgateway plugin selects a Gateway only when its
GatewayClass names `agentgateway.dev/gatewayclass-controller`. Solo
Enterprise for agentgateway registers `solo.io/enterprise-agentgateway`, so
its Gateways, routes and backends were missing from the graph.

**What it does:** `--agentgateway-controller-names` and
`--agentgateway-gateway-classes` take comma-separated lists; empty keeps the
defaults. Test: `TestCLIConfigControllerNames`.

## 0002: chart values for the model features and the rest

**Why.** The chart could point at a model endpoint but not turn on the
features that use it, name the model, or pass the flags above.

**What it does:** `model.name`, `model.reasoningEffort`,
`model.agenticInvestigation`, `model.deploymentRuleAuthoring`,
`ruleReviewAPI`, `discovery.agentgateway.controllerNames` and
`.gatewayClasses`, plus `extraArgs` and `extraEnv`. All default off or empty,
so existing installs render unchanged.

## 0003: portable JSON schemas on the Responses API

**Why.** Agentic investigation asks a Responses endpoint for an answer in a
strict JSON schema. Behind a gateway that serves Anthropic's models (the
lab's, on Bedrock), structured outputs reject `maxItems`, string lengths and
numeric bounds, so every investigation failed with a 400.

**What it does:** the investigation's text format gets the schema the
Anthropic provider already sends: those keywords move into descriptions, and
the validator enforces each after generation, as it already did. Test:
`TestJSONSchemaTextFormatIsPortable`.

## Build

`./build.sh` → `localhost:5001/lab/agentshield:<commit>-lab.<hash of this directory>`,
from the upstream Dockerfile. Layer 92 runs it when the image isn't in the
lab registry yet, and installs the chart from the same patched checkout
(`.lab/cache/agentshield-*`).
