# solo-lab

[![Runs on kind](https://img.shields.io/badge/runs%20on-kind%20%C2%B7%20k8s%201.37-326ce5?logo=kubernetes&logoColor=white)](docs/SETUP.md#requirements)
[![Editions](https://img.shields.io/badge/editions-OSS%20%7C%20Solo%20Enterprise-7b33ea)](docs/ENTERPRISE.md)
[![CI](https://github.com/nickgamb/solo-lab/actions/workflows/ci.yaml/badge.svg)](https://github.com/nickgamb/solo-lab/actions/workflows/ci.yaml)
[![License](https://img.shields.io/badge/license-Apache--2.0-8cc2d4)](LICENSE)

**An identity fabric built with [Solo.io](https://www.solo.io): every IdP a firm owns, through one
gateway, to every agent, tool, SaaS and model it uses.**

A production-shaped lab you run on your laptop. People and agents sign in once;
agentgateway routes each sign-in to the right IdP, fails over between IdPs
without anything downstream noticing, checks who and which workload is
calling on every request, and mints each tool or SaaS a token for that one
call. Everything is a real Solo product doing a real job, watched live in the
Solo UI.

| [![Solo UI, kagent: a trace of Bob's agent calling get_positions on Alice's terms](docs/images/solo-ui-kagent-trace.jpg)](docs/images/solo-ui-kagent-trace.jpg) | [![Solo UI, agentgateway: requests, tokens by model and tool requests across the lab](docs/images/solo-ui-agentgateway-dashboard.jpg)](docs/images/solo-ui-agentgateway-dashboard.jpg) | [![Solo UI, Istio: the ambient mesh by namespace, with live traffic](docs/images/solo-ui-mesh-graph.jpg)](docs/images/solo-ui-mesh-graph.jpg) |
| :---: | :---: | :---: |
| **kagent**: every agent run, traced to the tool | **agentgateway**: every call, model token and tool request | **Istio ambient**: the mesh, live |

And in the lab's own Observatory, the identity fabric: agents and apps through
agentgateway to every IdP the firm owns, in failover order.

![The identity fabric in the Observatory: agents and apps through agentgateway, the fabric, and S&V's IdPs in failover order](docs/images/observatory-continuity-connected.jpg)

## What it shows

- **Sign-in routing at the gateway.** Rules in CEL send each sign-in to the
  right IdP and fall back when it's down. [Identity fabric card](docs/cards/identity-fabric.html)
- **IdP failover, for real.** Cut the active IdP's network and sign-in moves
  to the next one; same issuer, same user, nothing downstream changes.
  [Identity continuity card](docs/cards/identity-continuity.html)
- **Assurance that survives failover.** Each workload says what a sign-in
  must prove; fail over to an IdP that proves less and its requests are
  refused (RFC 9470 step-up), decided by Solo's ext-auth service.
  [Assurance rules card](docs/cards/assurance.html)
- **Agents acting for people.** Bob's agent never holds a token an MCP server
  accepts: the gateway mints one per call, enforces per-tool policy, and
  reaches a SaaS with Cross App Access (ID-JAG). [Bob card](docs/cards/bob.html)
- **A client's data on her terms.** The same agent asks Alice; she approves
  each new tier and can deny a trade ([UMA for agents](https://u4a.ai)). [Bob to Alice card](docs/cards/bob-to-alice.html)
- **Models behind the same gateway.** Provider failover, prompt guards,
  masking, budgets and per-call cost. [Observatory: Model Continuity](docs/OBSERVATORY.md#model-continuity)

Cards are two-screen demo scripts: open them in a browser (`open docs/cards/<card>.html`).

## Architecture

![Callers, the Solo gateways every call goes through, what they reach, the identity fabric beneath, and Istio ambient under everything](docs/images/architecture.svg)

| Product | In this lab |
| --- | --- |
| [agentgateway](https://www.solo.io/products/agentgateway) | the firm's AI gateway: sign-in routing, the front door for every MCP server, a short-lived token per call, Cross App Access, models; Meridian's gateway |
| [kgateway](https://www.solo.io/products/kgateway) | the edge: TLS, one listener per party, SSO for every UI |
| [kagent + kmcp](https://www.solo.io/products/kagent) | agents, their controller and UI; MCP servers as Kubernetes resources |
| [Agent Substrate](https://www.solo.io/products/kagent) | every agent runs in a snapshot-backed sandbox on a worker pool |
| [agentregistry](https://www.solo.io/products/agentregistry) | the firm's catalog of agents, MCP servers and skills |
| [Istio ambient](https://www.solo.io/products/istio) | mTLS and a SPIFFE identity for every workload, waypoints, default-deny per party |

Four parties share the platform (an advisory firm, its SaaS vendor, a client
and her brokerage), each with its own namespaces, identities, IdP and
hostnames, meeting only at the edge. Every product runs OSS by default and
switches to Solo Enterprise on its own ([ENTERPRISE.md](docs/ENTERPRISE.md)).
Details: [ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Quick start

You need Docker with 24 GB or more, kind, kubectl, helm, jq, yq and a model
(Ollama on the host, or an Anthropic or OpenAI key). The full list, per OS:
[SETUP.md](docs/SETUP.md#requirements).

```bash
git clone https://github.com/nickgamb/solo-lab && cd solo-lab
cp .env.example .env      # optional: model provider, keys, IdPs, Solo Enterprise
make machine-setup        # once per machine, sudo: *.lab DNS and the lab CA
make up                   # cluster, platform and every demo: ~30 min cold, ~17 GB
make status               # pods, the active IdP, URLs and sign-ins
```

Then open the Observatory at https://observatory.ops.lab (`ops` / `ops-demo`)
and run every check:

```bash
make verify               # rewinds the demos, then every enforcement check
```

`make down` deletes the cluster and keeps the image caches and the CA, so the
next `make up` is much faster; `make nuke` removes those too.

## What you can open

| URL | What | Sign in |
| --- | --- | --- |
| https://observatory.ops.lab | **Observatory**: topology, traffic with each hop's verified claims, identity and model continuity | `ops` / `ops-demo` |
| https://kagent.sterling.lab/ke/ | **Solo UI · kagent** (Enterprise): agents, chats, traces, tools, models. On OSS, https://kagent.sterling.lab is kagent's own UI | S&V sign-in: `bob` (advisor) or `dana` (admin) |
| https://kagent.sterling.lab/age/ | **Solo UI · agentgateway** (Enterprise): gateways, routes, policies, traces, cost, playground | `dana` |
| https://kagent.sterling.lab/ie/graph | **Solo UI · Istio** (Enterprise): the mesh graph, global services, resources | `dana` |
| https://registry.sterling.lab | **agentregistry**: the firm's catalog, and the kagent runtime | S&V sign-in |
| https://portal.alice.lab | Alice's portal: her grants and terms | `alice` / `alice-demo` |
| https://grafana.ops.lab, https://kiali.ops.lab | Grafana; the Kiali mesh graph (view-only) | `ops` / `ops-demo` |
| https://idp.sterling.lab/realms/sterling-vance/account | S&V's broker: your account, signed in through the active IdP | S&V sign-in |
| https://mcp.sterling.lab/mcp/bob-workspace | the firm's MCP front door for AI clients (Claude Code, an IDE): add it as a remote MCP server | S&V sign-in |
| https://llm.sterling.lab/v1 | the firm's model route: OpenAI chat completions or Anthropic messages | API key `LLM_API_KEY` in `.lab/secrets.env` |

S&V sign-in goes to the active IdP: S&V's own Keycloak unless you add an
enterprise IdP such as Auth0 in `.env`, with `bob` / `bob-demo` and a one-time
code from `make totp`
(`make totp EMPLOYEE=dana` for Dana). Every account, and the APIs on the edge:
[SETUP.md](docs/SETUP.md#accounts) and [COMMANDS.md](docs/COMMANDS.md).

## See it

**The Solo UI** ([all of it](docs/ENTERPRISE.md#the-solo-ui)):

| | |
| --- | --- |
| ![Solo UI, kagent: agents, runs, tokens per model, the cluster](docs/images/solo-ui-kagent-dashboard.jpg) | ![Solo UI, kagent: every agent, its model and its tools](docs/images/solo-ui-kagent-agents.jpg) |
| **kagent**: agents, runs and tokens | **kagent**: every agent, its model and tools |
| ![Solo UI, agentgateway: every route, its match, destination and latency](docs/images/solo-ui-agentgateway-routes.jpg) | ![Solo UI, agentgateway: cost management, spend by provider and over time](docs/images/solo-ui-agentgateway-cost.jpg) |
| **agentgateway**: every route behind the AI gateway | **agentgateway**: cost per call, by provider |

**The Observatory**, the lab's own view of identity and model continuity:

| | |
| --- | --- |
| ![IdP failover: S&V's Keycloak cut at the egress, the contingency IdP signing people in](docs/images/observatory-continuity-failover.jpg) | ![What if: a contingency sign-in (AAL1) refused where the rule needs AAL2, with the reason](docs/images/observatory-assurance-whatif.jpg) |
| **IdP failover**: a real network cut, the next IdP takes over | **Assurance what-if**: refused below AAL2, and why |
| ![Model failover: the primary model down, the fallback serving every agent](docs/images/observatory-model-failover.jpg) | ![Routing policy: which IdP each sign-in goes to, as rules in CEL](docs/images/observatory-routing-policy.jpg) |
| **Model failover** at the gateway | **Routing policy**: rules in CEL, decided at the gateway |

More in [OBSERVATORY.md](docs/OBSERVATORY.md), and the
[3-minute walkthrough](https://youtu.be/Y3P4a7HRvVs).

## Docs

| Doc | Covers |
| --- | --- |
| [SETUP.md](docs/SETUP.md) | requirements, machine setup, accounts, configuration, every `make` target, layout and layers |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | parties, workloads and identities, enforcement layers, DNS and TLS, patches |
| [IDENTITY-FLOWS.md](docs/IDENTITY-FLOWS.md) | acting for a user (RFC 8693), Cross App Access (ID-JAG), UMA for agents: every hop, policy and check |
| [IDENTITY-CONTINUITY.md](docs/IDENTITY-CONTINUITY.md) | the IdP chain, the IdentityContinuity API, directory sync, trust across IdPs, assurance rules, the kill switch |
| [OBSERVATORY.md](docs/OBSERVATORY.md) | the Observatory's tabs, how it builds its map, its access model |
| [COMMANDS.md](docs/COMMANDS.md) | terminal commands for every part of the running lab |
| [ENTERPRISE.md](docs/ENTERPRISE.md) | switching products to Solo Enterprise |
| [IDPS.md](docs/IDPS.md) | adding Auth0, Gluu, Okta, Ping or any OIDC IdP; Ledgerline's authorization server |
| [GLUU.md](docs/GLUU.md) | Gluu (experimental) as S&V's IdP and Ledgerline's authorization server |

Solo Enterprise editions, trial licences and support: [solo.io/get-started](https://www.solo.io/get-started).

## License

This repository's own code and docs: [Apache-2.0](LICENSE). The Solo products
it installs, and the upstream projects patched in `tools/`, remain under their
own terms: see [NOTICE](NOTICE).
