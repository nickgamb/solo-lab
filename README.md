# solo-lab

[![Kubernetes 1.37 on kind](https://img.shields.io/badge/runs%20on-kind%20%C2%B7%20k8s%201.37-326ce5?logo=kubernetes&logoColor=white)](docs/ARCHITECTURE.md#install-order)
[![Mesh Istio ambient 1.31](https://img.shields.io/badge/mesh-Istio%20ambient%201.31-466bb0?logo=istio&logoColor=white)](docs/ARCHITECTURE.md#enforcement-layers)
[![Gateways](https://img.shields.io/badge/gateways-kgateway%20%C2%B7%20agentgateway-5e8fa3)](docs/ARCHITECTURE.md#enforcement-layers)
[![Agents](https://img.shields.io/badge/agents-kagent%20%C2%B7%20Agent%20Substrate-5e8fa3)](docs/ARCHITECTURE.md#workloads-and-identities)
[![Identity](https://img.shields.io/badge/identity-RFC%208693%20%C2%B7%20ID--JAG%20%C2%B7%20UMA%202.0-8cc2d4)](docs/IDENTITY-FLOWS.md)
[![Editions](https://img.shields.io/badge/editions-OSS%20%7C%20Solo%20Enterprise-8cc2d4)](docs/ENTERPRISE.md)
[![Companion u4a.ai](https://img.shields.io/badge/companion-u4a.ai-bcdb2c)](https://u4a.ai)

A local, production-shaped lab for the Solo.io AI platform on kind: Istio
ambient, kgateway, agentgateway, kagent + kmcp, Agent Substrate,
agentregistry and Keycloak, plus two lab apps: the **Observatory** (a live
control-plane UI) and an **identity continuity** controller (IdP failover).
Everything runs OSS; each product can switch to Solo Enterprise on its own
([docs/ENTERPRISE.md](docs/ENTERPRISE.md)).

![The whole lab in the Observatory: the edge on the left, one lane per party, external services on the right](docs/images/observatory-export-topology-all.jpg)

The lab models four parties on a shared platform (an advisory firm, its SaaS
vendor, a client and her brokerage), each with its own namespaces, SPIFFE
identities, IdP and hostnames. They meet only at the edge. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Requirements

- macOS (built and tested on Apple silicon; the DNS resolver and CA trust steps are macOS-specific)
- Docker Desktop with 24 GB or more for its VM (`make preflight` refuses under 16 GB; the full lab uses about 17 GB)
- `brew install kind kubectl helm jq yq gettext openssl node`, with kind v0.32 or newer
- Ports 80 and 443 free on 127.0.0.1, or set `LAB_HTTP_PORT`/`LAB_HTTPS_PORT` in `.env`
- A model: [Ollama](https://ollama.com) on the host (`ollama pull qwen3.8:27b`), or an Anthropic or OpenAI key

`make preflight` checks all of this. It also raises the Docker VM's inotify
limits, which reset whenever Docker Desktop restarts.

## Quick start

```bash
cp .env.example .env          # optional: model provider, keys, Auth0, enterprise
make machine-setup            # once per Mac, one sudo: *.lab DNS and the lab CA
make up                       # cluster, platform and every demo (~30 min cold)
make verify                   # every demo's enforcement checks
make status                   # pods, active IdP, URLs and sign-ins
```

Story 2 builds Alice's and Meridian's services from
[uma4agents](https://github.com/nickgamb/uma4agents): a checkout beside this
repo (`../uma4agents`) or `U4A_SRC` if you have one, otherwise the install
clones the pinned commit into `.lab/uma4agents`.

`make up` is idempotent. Image caches and the lab CA outlive the cluster, so
a rebuild after `make down` is much faster than the first.

What it leaves running in Docker: pull-through mirrors for each upstream
registry (`lab-mirror-*`), a push registry for lab-built images
(`lab-registry`, `localhost:5001`), `lab-dns` (answers `*.lab` on port
15353), `cloud-provider-kind`, and the kind nodes (one control plane, three
workers in zones a, b and c). The full lab uses about 17 GB of memory.

![Docker Desktop with the lab running](docs/images/docker-desktop-lab-containers.jpg)

## What you can open

| URL | What | Sign in |
| --- | --- | --- |
| https://observatory.ops.lab | Observatory: topology, traffic, identity continuity | `ops` / `ops-demo` |
| https://kagent.sterling.lab | kagent, where Bob's agents run | `bob` / `bob-demo` (or Bob's upstream IdP account) |
| https://registry.sterling.lab | agentregistry | S&V sign-in |
| https://portal.alice.lab | Alice's portal (her grants and terms) | `alice` / `alice-demo` |
| https://grafana.ops.lab | Grafana | `admin` / `solo-lab` |
| https://kiali.ops.lab | Kiali mesh graph | none |
| https://idp.sterling.lab | S&V Keycloak | admin password in `.lab/secrets.env` |

Generated secrets (Keycloak admin passwords, client secrets) are in
`.lab/secrets.env`, created on first install and gitignored.

## Demos

Each demo has a card: a two-screen script of what to do and what to say.
Open it in a browser (`open docs/cards/<card>.html`).

| Card | Shows | Checks |
| --- | --- | --- |
| [Bob](docs/cards/bob.html) | Bob's agent acts for Bob: RFC 8693 delegation at the MCP waypoint, per-tool policy, human approval for writes, Cross App Access (ID-JAG) to a SaaS | `make bob-verify` |
| [Bob to Alice](docs/cards/bob-to-alice.html) | the same agent asks Alice for her data on her terms (UMA for agents) | `make alice-verify` |
| [Observatory tour](docs/cards/observatory.html) | every story end to end from one command, watched live: the agent waking, verified tokens per hop, refusals, Alice's terms, an IdP outage | `make tour` |
| [Identity continuity](docs/cards/identity-continuity.html) | a real network outage of the upstream IdP, automatic failover to local accounts, and failback, live in the Observatory | `make continuity-verify` |

`make reset` rewinds every demo without a rebuild. The identity continuity
demo needs an Auth0 tenant (free tier is enough):
[docs/IDENTITY-CONTINUITY.md](docs/IDENTITY-CONTINUITY.md#auth0-setup).

## Observatory

A single pane of glass over the running lab, built from the cluster at
runtime, so a different lab renders the same way.

- **Topology:** every workload by zone and call stage, live traffic on the
  wires, a details panel per node with its live YAML (edit and apply), four
  views (All, Identity, Agents & tools, Cross-party), and PNG export.
- **Traffic:** every gateway's access log, with the verified token claims on
  each request.
- **Identity Continuity:** the sign-in chain, the outage button, and the rule
  builder.

[![Observatory walkthrough at 4x speed: topology views, hover, details and live YAML, traffic with token claims, and an IdP outage with failover and failback](docs/videos/observatory.gif)](https://youtu.be/Y3P4a7HRvVs)

The full walkthrough at normal speed, on YouTube: [Solo.io Observatory](https://youtu.be/Y3P4a7HRvVs) (3 min).

See [docs/OBSERVATORY.md](docs/OBSERVATORY.md).

## Configuration

`.env` (copy `.env.example`, gitignored) holds everything personal:

| Key | Used for |
| --- | --- |
| `LLM_PROVIDER` | `ollama` (default), `anthropic` or `openai`; apply with `make llm` |
| `OLLAMA_MODEL`, `OLLAMA_URL` | the local model and where the cluster reaches it |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL`, `OPENAI_API_KEY`, `OPENAI_MODEL` | hosted models (held by agentgateway only) |
| `AUTH0_CLIENT_ID`, `AUTH0_CLIENT_SECRET` | S&V's upstream workforce IdP; apply with `make layer-47` |
| `EDITION`, `<PRODUCT>_EDITION`, `SOLO_LICENSE_KEY` | Solo Enterprise, all products or one at a time |

`config/lab.env` holds the lab's shape: cluster name, node image, worker
count, host ports, party domains, `AUTH0_ISSUER`, registry port. Any value
can be overridden in `.env` or the shell.

## Make targets

| Target | Does |
| --- | --- |
| `machine-setup` | one-time: `*.lab` resolver and lab CA trust (one sudo) |
| `up` | `cluster` then `platform` |
| `cluster` | kind cluster, registry caches, cloud-provider-kind, lab DNS |
| `platform` | every layer under `platform/` in order |
| `layer-NN` | one layer, e.g. `make layer-90` (Observatory) |
| `verify` | `bob-verify`, `alice-verify`, `continuity-verify` |
| `tour` | drive every story end to end, paced, to watch in the Observatory |
| `reset` | rewind the demos (grants, terms, agent key, follow-ups) |
| `llm` | switch the model: `make llm LLM_PROVIDER=anthropic` |
| `status` | pods, the active sign-in tier, URLs |
| `preflight` | tools, Docker resources, Ollama |
| `down` | delete the cluster (keeps caches and the CA) |
| `nuke` | delete the cluster and the image caches |

## Layout

```
config/          lab shape (lab.env) and edition pins (oss.env, enterprise.env)
scripts/         cluster lifecycle, DNS, CA, helpers (lib.sh), status, reset, llm
platform/NN-*/   one install.sh per layer, applied in order by make platform
demos/           each story: install.sh, verify.sh, manifests, agents, tools
apps/observatory Observatory: Go server (server/) and React UI (web/)
apps/continuity  IdentityContinuity CRD and controller
tools/           patched upstream builds (kagent, Substrate, Keycloak), toolbox image
docs/            architecture, per-app docs, gotchas, demo cards
```

Layers:

| Layer | Installs |
| --- | --- |
| `00-foundation` | Gateway API, metrics-server, cert-manager, trust-manager, lab CA, namespaces |
| `10-istio` | Istio ambient: base, istiod, istio-cni, ztunnel |
| `20-observability` | kube-prometheus-stack, Tempo, OTel collector, Kiali |
| `30-kgateway` | the edge: per-party TLS listeners on NodePorts 30080/30443 |
| `40-agentgateway` | ai-gateway: LLM backend, MCP, A2A |
| `45-identity` | S&V Keycloak and the client secrets S&V components use |
| `47-continuity` | IdentityContinuity CRD and controller, S&V egress waypoint |
| `50-substrate` | Agent Substrate (patched), in the mesh |
| `60-kagent` | kagent + kmcp (patched), ops agents on Substrate, edge SSO |
| `70-agentregistry` | agentregistry behind S&V SSO |
| `80-mesh-policy` | S&V's mesh baseline |
| `90-observatory` | Observatory, its Keycloak (realm `ops`), gateway access logs |
| `95-demos` | every story: `demos/bob`, then `demos/bob-to-alice` |

## Docs

| Doc | Covers |
| --- | --- |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | parties, workloads and identities, enforcement layers, DNS and TLS, patches |
| [IDENTITY-FLOWS.md](docs/IDENTITY-FLOWS.md) | delegation (RFC 8693), Cross App Access (ID-JAG), UMA for agents: every hop, policy and check |
| [OBSERVATORY.md](docs/OBSERVATORY.md) | using the Observatory, how it derives the map, access model, local development |
| [IDENTITY-CONTINUITY.md](docs/IDENTITY-CONTINUITY.md) | the IdentityContinuity API, the controller, Auth0 setup, the kill switch |
| [ENTERPRISE.md](docs/ENTERPRISE.md) | switching products to Solo Enterprise |
