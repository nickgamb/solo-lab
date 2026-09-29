# Architecture: who owns what

The lab is four **parties** on one cluster, plus the neutral platform they all
run on. Each party gets its own namespaces, its own service accounts (so its
own SPIFFE identities), its own IdP where the story needs one, and its own
hostnames. Parties never talk pod-to-pod: they meet at the **edge**, the way
separate companies meet on the internet. That is enforced by mesh identity,
not by convention.

| Party | Role in the stories | Domain | Namespaces |
| --- | --- | --- | --- |
| **Platform** | the "cloud": mesh, edge, telemetry, substrate, and the Observatory | `ops.lab` | `istio-system` `kgateway-system` `observability` `ate-system` `cert-manager` `observatory` `ops-identity` |
| **Sterling & Vance** | Bob's firm. Runs the Solo AI platform (kagent, agentgateway, agentregistry) for its advisors | `sterling.lab` | `sv-identity` `sv-egress` `kagent` `agentgateway-system` `agentregistry` `sv-agents` `sv-mcp` `sv-u4a` |
| **Alice** | resource owner. Her authorization server, her portal, her IdP | `alice.lab` | `alice` `alice-identity` |
| **Meridian Wealth** | Alice's brokerage. Holds her account, enforces her terms, can never read them | `meridian.lab` | `meridian` |
| **Ledgerline Research** | a SaaS S&V subscribes to (Cross App Access target) | `ledgerline.lab` | `ledgerline` `ledgerline-identity` |

Bob is a user of Sterling & Vance (realm `sterling-vance` in `sv-identity`).
S&V's Keycloak may broker his sign-in to an upstream workforce IdP (Auth0),
but it stays the only issuer anything trusts (see Identity continuity).
Alice is a user of her own IdP (realm `alice` in `alice-identity`).

## Workloads and identities

Every workload has its own ServiceAccount. The SPIFFE ID is
`spiffe://cluster.local/ns/<ns>/sa/<sa>`; policies below name these.

| Namespace | Workload | SA | Owner | Reached by |
| --- | --- | --- | --- | --- |
| `kgateway-system` | edge (kgateway/Envoy) | `edge` | platform | laptop (NodePort 30080/30443) |
| `observatory` | Observatory (reads the cluster; writes only by impersonating the signed-in admin) | `observatory` | platform | edge (UI/API); otel-collector (OTLP logs); `ops-identity` Keycloak |
| `ops-identity` | Keycloak `ops` (platform admins, for the Observatory) | `keycloak` | platform | edge; the Observatory (JWKS) |
| `sv-identity` | Keycloak `sterling-vance` | `keycloak` | S&V | edge; S&V gateways/apps (JWKS, token exchange); continuity-controller (admin API) |
| `sv-identity` | `continuity-controller` (2 replicas, leader-elected) | `continuity-controller` | S&V | nobody (calls out only) |
| `sv-egress` | `egress-waypoint` (Istio waypoint for external upstream IdPs) | `egress-waypoint` | S&V | Keycloak and continuity-controller only |
| `kagent` | controller, UI, oauth2-proxy, tools | `kagent-*` | S&V | edge → oauth2-proxy → UI → controller |
| `kagent` | ops agents (k8s, istio, helm, promql, kgateway): SandboxAgents on pool `kagent-ops` | `kagent-ops` | S&V | atenet-router only |
| `ate-system` | Agent Substrate: ate-api, atenet-router, atelet, ate-controller, valkey, rustfs | one SA per component | platform | ate-api and router: kagent controller only (ate-api also the Observatory); the rest: `ate-system` only |
| `agentgateway-system` | ai-gateway (LLM + MCP) | `ai-gateway` | S&V | S&V agents; edge (`ai.sterling.lab`, JWT required) |
| `agentregistry` | agentregistry | `agentregistry` | S&V | edge via ai-gateway (JWT required); kagent controller |
| `sv-agents` | `bob-assistant`: SandboxAgent on pool `bob-assistant` | `bob-assistant` | S&V / Bob | atenet-router only (kagent controller → ate-api → router) |
| `sv-agents` | advisor desk (meeting-prep, market-brief, compliance-check): SandboxAgents on pool `advisor-desk` | `advisor-desk` | S&V | atenet-router only |
| `sv-mcp` | `bob-workspace` (kmcp) | `bob-workspace` | S&V / Bob | **ai-gateway only** (waypoint also checks the delegated token) |
| `sv-mcp` | `mcp-waypoint` (agentgateway as the namespace's waypoint) | `mcp-waypoint` | S&V | every caller of S&V tools, via ztunnel |
| `sv-u4a` | `u4a-adapter` (UMA client: holds Bob's agent's key) | `u4a-adapter` | S&V / Bob | Bob's agent and the kagent controller only |
| `ledgerline-identity` | Keycloak `ledgerline` (ID-JAG receiver) | `keycloak` | Ledgerline | edge |
| `ledgerline` | `ledgerline-research` (kmcp) behind an Istio waypoint | `ledgerline-research` | Ledgerline | edge (Ledgerline token for calls) |
| `alice-identity` | Keycloak `alice` | `keycloak` | Alice | edge; `alice/uma-as` (JWKS) |
| `alice` | uma-as (Alice's AS) | `uma-as` | Alice | edge (grant surface); `meridian/uma-pep` (protection API); portal (owner API) |
| `alice` | alice-portal | `portal` | Alice | edge |
| `meridian` | meridian gateway (agentgateway) | `meridian` | Meridian | **edge only** |
| `meridian` | uma-pep (ext-auth) | `uma-pep` | Meridian | meridian gateway only |
| `meridian` | alice-vault (kmcp) | `alice-vault` | Meridian, holding Alice's account | meridian gateway; Alice's portal |

Agents run on Agent Substrate: one snapshot-backed gVisor actor per
conversation, on a worker pool. An actor's calls leave through its worker pod,
so a pool runs as the ServiceAccount of the agent it serves and the actor's
SPIFFE ID is that one (`tools/substrate-mesh`). A pool with its own
ServiceAccount only runs its own namespace's actors. Turns reach an actor only
as kagent controller → atenet-router → worker, each hop mTLS.

## Enforcement layers

1. **ztunnel (L4, every pod).** Mesh-wide STRICT mTLS. Each party namespace
   is default-deny; ALLOW rules name SPIFFE principals from the table above.
2. **Waypoints (L7, per party namespace).** `sv-mcp`, `alice`, `meridian`
   and the identity namespaces each run a waypoint (`istio.io/waypoint-for:
   all`). Path- and JWT-based rules live there and bind with `targetRefs`,
   never `selector`: a selector policy with L7 attributes lands on ztunnel and
   becomes a deny.
3. **Gateways (L7, per request).**
   - Edge (kgateway): TLS, one listener per party hostname, and each
     listener only accepts routes from that party's namespaces.
   - ai-gateway (agentgateway, S&V): JWT validation against S&V's Keycloak,
     per-tool MCP authorization in CEL, RFC 8693 token exchange / ID-JAG
     toward tools, provider credentials for LLMs.
   - meridian gateway (agentgateway, Meridian): ext-auth to uma-pep, which
     enforces Alice's terms (UMA tickets, PoP RPTs, single-use grants).

## Identity continuity

`IdentityContinuity` (`kubectl get idc -n sv-identity`) is an ordered chain
of upstream IdPs for S&V's Keycloak, ending in its own accounts. The
continuity controller probes every tier and points Keycloak's login at the
first healthy one; Keycloak stays the issuer everything trusts, and brokered
users are linked to their S&V user by verified email, so `sub` never changes.
External upstreams are reached through `sv-egress/egress-waypoint` (one
ServiceEntry per tier), which is also where an outage is simulated: a DENY
policy on that ServiceEntry. Details, the API and Auth0 setup:
[IDENTITY-CONTINUITY.md](IDENTITY-CONTINUITY.md).

## Observability and the Observatory

Every component sends OTLP to one collector (`observability/otel-collector`):
traces to Tempo, metrics to Prometheus, gateway access logs to the
Observatory. Prometheus also scrapes ztunnel, waypoints and gateways, which is
where the Observatory's observed edges come from. Kiali and Grafana read the
same data. The Observatory derives its map from the cluster's own objects;
see [OBSERVATORY.md](OBSERVATORY.md).

## Naming and DNS

Every party hostname is `<name>.<party>.lab`, and it resolves the same way
everywhere, so an issuer URL means the same thing to a browser, a CLI and a
pod:

- **Mac:** `/etc/resolver/lab` sends `*.lab` to the lab DNS container on
  `127.0.0.1:15353`, which answers `127.0.0.1`. `make machine-setup` writes
  it (one-time sudo).
- **Cluster:** CoreDNS rewrites `*.lab` to the edge Service.
- **Waypoints:** `sv-mcp` uses an agentgateway waypoint (MCP-aware). `alice`,
  `meridian` and `ledgerline` use Istio waypoints, opted into per workload
  only where a path-level rule needs one.
- **TLS:** the lab CA is generated once into `~/.solo-lab/ca` and outlives
  clusters, so you trust it once (`make machine-setup`). It is name-constrained to
  `.lab`, `.svc` and `.cluster.local`. In-cluster
  consumers get it through trust-manager (`lab-ca-bundle` ConfigMap).

Why not `*.localhost`: agentgateway's resolver (hickory, RFC 6761) and newer
Go resolvers answer `*.localhost` with loopback without asking DNS. A
`keycloak.localhost` issuer would then mean "the pod itself" inside the
cluster.

## Demos build on each other

Bob (story 1) is the foundation. Each later story adds its own parties and at
most a kustomize overlay on Bob's agent (`demos/<story>/agent`). One `make up`
installs every story, so any card runs in any order on the same lab.
`make verify` runs every story's checks; `make reset` rewinds all of them.

## Patches (tracked for upstream)

| Where | What | Upstream |
| --- | --- | --- |
| `tools/keycloak-idjag` | Keycloak 26.7.4 + PR #49998 (ID-JAG issuing), backported | keycloak/keycloak#49998 |
| `tools/kagent` 0001 | kagent 0.10.2: forward the user's ID token to agents (bound to the user) | kagent-dev/kagent (PR to open) |
| `tools/kagent` 0002 | Go ADK: SandboxAgents call the controller back with the caller's credential (they have no ServiceAccount token) | kagent-dev/kagent (PR to open) |
| `tools/kagent` 0003 | a SandboxAgent turn sent as the last one closes (HITL approval) no longer races its suspend | kagent-dev/kagent (PR to open) |
| `tools/substrate-mesh` 0001 | Substrate 0.0.9: WorkerPool pod identity (`serviceAccountName`, labels, annotations) | kagent-dev/substrate (PR to open) |
| `tools/substrate-mesh` 0002 | a pool with its own ServiceAccount only runs its namespace's actors | kagent-dev/substrate (PR to open) |
| `tools/substrate-mesh` 0003 | ateom: inbound relayed from a local socket, worker-local traffic pinned, so actors work under in-pod mesh capture | kagent-dev/substrate (PR to open) |

## Install order

```
00-foundation   Gateway API, metrics-server, cert-manager, trust-manager, lab CA, namespaces
10-istio        ambient: base, istiod (HA), cni, ztunnel
20-observability kube-prometheus-stack, Tempo, OTel collector, Kiali
30-kgateway     edge (HA, pinned NodePorts, per-party TLS listeners)
40-agentgateway ai-gateway (HA, mesh-native), LLM backend (make llm)
45-identity     S&V Keycloak, DNS rewrite, SSO for kagent/Grafana/Kiali
47-continuity   upstream IdP failover for S&V Keycloak (IdentityContinuity, controller, egress waypoint)
50-substrate    Agent Substrate (patched, ate-system in the mesh)
60-kagent       kagent + kmcp, model via ai-gateway, ops agents on Substrate
70-agentregistry agentregistry behind S&V SSO at the edge
80-mesh-policy  S&V mesh baseline (other parties own theirs, in their story)
90-observatory  Observatory, its Keycloak (realm ops), gateway access logs to it
95-demos        every story: bob, then bob-to-alice
```
