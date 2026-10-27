# Agentshield

[Agentshield](https://agent-shield.dev) is Solo's open source security
service for Kubernetes: it learns how workloads talk, flags lateral movement
with the evidence behind it, and proposes fixes for an operator to approve.
The lab installs it the way a platform team would run it next to Istio and
agentgateway: its own namespace and database, its console behind the admins'
sign-in, the waypoints' traffic through the collector, and its model calls
through the AI gateway. It's on by default.

## What it does here

| | |
| --- | --- |
| **Graph** | every workload, ServiceAccount, Service, gateway, route, AuthorizationPolicy, PeerAuthentication and ServiceEntry, and how they relate: from the Kubernetes API, read-only |
| **Traffic** | each request an Istio waypoint serves (who called which Service, how, with what result), refused ones included |
| **Detection** | new paths and exposures against what it has seen before; built-in rules, and rules it drafts per Deployment |
| **Investigation** | a model looks into each new path with read-only graph tools and returns a finding with evidence, or nothing |
| **Rule drafts** | after an hour of a Deployment's traffic, up to three narrow rules for it, in shadow mode until an admin approves them |

## Open it

https://agentshield.ops.lab, as `ops` / `ops-demo` (the Observatory's
sign-in).

| Page | Shows |
| --- | --- |
| Overview | graph size, newest findings, and each source's health (Kubernetes, Istio, agentgateway) |
| Environment graph | the graph, around any resource |
| Catalog | every resource with its relationships and facets |
| Findings | findings with their evidence and proposed fixes |
| Rules | loaded rules, and drafts to approve or reject |

Fixes are proposed, not applied: the lab gives Agentshield read-only access,
so an approved fix is one to make in the lab's manifests.

## How it works

```
Kubernetes API ──(read-only)──▶ Agentshield ◀──OTLP── otel-collector ◀── waypoints' access logs
                                    │
                                    ├──▶ agentshield-db (CloudNativePG)
                                    └──▶ ai-gateway /security/v1 ──▶ the security tools' model

admins ──▶ edge (ops sign-in) ──▶ Agentshield console
```

- **Image and chart.** Agentshield publishes no release yet. Layer 92
  builds its Dockerfile and installs its own Helm chart from the commit in
  `config/oss.env` (`AGENTSHIELD_COMMIT`), with the lab's patches
  ([tools/agentshield](../tools/agentshield/README.md)): Solo Enterprise for
  agentgateway's Gateways in the graph, the chart's model settings, and
  schemas Claude's structured output accepts.
- **Traffic.** Istio's waypoints log each request for Agentshield
  (`platform/10-istio/istiod.yaml`, provider `agentshield`) to the
  collector, which sends on those whose target is a Service in the cluster
  (`platform/20-observability/otel-collector-agentshield.yaml`). A request a
  waypoint refused never reached its target, so the collector names the
  target from the waypoint's route. Requests through agentgateway are in the
  graph as routes and policy, not as traffic.
- **Model.** Agentshield calls `http://ai-gateway.agentgateway-system/security/v1`,
  a route of its own (`platform/40-agentgateway/llm/security.yaml`) with a
  model that answers in a JSON schema: Claude Haiku 4.5 on Bedrock, Claude
  Haiku 4.5 on Anthropic, or the provider's model on OpenAI and Ollama
  (`LLM_SECURITY_MODEL` picks another). Only Agentshield's identity may use
  it, and it holds no key. Its calls show in the gateway's traces, cost and
  Datadog with every other model call. Investigations are capped at 30 an
  hour.
- **Access.** Its API has no authentication of its own. The edge signs
  every console request in (ops realm, client `agentshield`), and the mesh
  takes connections only from the edge and the collector. Its database takes
  only Agentshield and the operator. It has no internet.

## Check it

`make agentshield-verify`: the graph is published with the lab's
agentgateway Gateways in it, a waypoint's request reaches it, the console
asks for sign-in, and nothing else in the mesh can reach its API or its
model route.

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab -n agentshield-system get pods,cluster` | Agentshield and its database |
| `kubectl --context kind-solo-lab -n agentshield-system logs deploy/agentshield --since=10m` | discovery, detection, investigations and rule drafts |
| `kubectl --context kind-solo-lab -n agentgateway-system logs deploy/ai-gateway --since=10m \| grep llm-security` | its model calls: model, tokens, latency |
| `kubectl --context kind-solo-lab -n agentshield-system port-forward deploy/agentshield 8088` | its API at http://localhost:8088/api/v1/overview |

## Turn it off

`AGENTSHIELD=off` in `.env`, then `make layer-92` (removes it), and
`make layer-10 layer-20` (the waypoints and collector stop sending to it).
