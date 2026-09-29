# Architecture: who owns what

The lab is four **parties** on one cluster, plus the neutral platform they all
run on. Each party gets its own namespaces, its own service accounts (so its
own SPIFFE identities), its own IdP where the story needs one, and its own
hostnames. Parties never talk pod-to-pod: they meet at the **edge**, the way
separate companies meet on the internet. That is enforced by mesh identity,
not by convention ([zero trust](https://www.solo.io/topics/security-and-compliance/zero-trust)).

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
| `observatory` | Observatory (reads the cluster; writes only by impersonating the signed-in admin; reads Substrate status from kagent as its own S&V service account) | `observatory` | platform | edge (UI/API); otel-collector (OTLP logs) |
| `ops-identity` | Keycloak `ops` (platform admins, for the Observatory) | `keycloak` | platform | edge (the realm only); the Observatory (JWKS) |
| `observability` | otel-collector, Prometheus, Tempo, Grafana | one SA per component | platform | collector: the components that report telemetry, by ServiceAccount; Prometheus: the collector, Tempo, Grafana, Kiali, the Observatory; Tempo: the collector, Grafana, Kiali; Grafana: edge (after sign-in), Kiali, Prometheus |
| `kiali` | Kiali (view-only) | `kiali` | platform | edge (after sign-in); Prometheus (metrics) |
| `sv-identity` | Keycloak `sterling-vance` | `keycloak` | S&V | edge (the realm only); S&V gateways/apps and `bob-workspace` (JWKS, token exchange); continuity-controller (admin API) |
| `sv-identity` | `continuity-controller` (2 replicas, leader-elected) | `continuity-controller` | S&V | nobody (calls out only) |
| `sv-egress` | `egress-waypoint` (Istio waypoint for external upstream IdPs) | `egress-waypoint` | S&V | Keycloak and continuity-controller only |
| `kagent` | controller, UI, tools | `kagent-*` | S&V | UI: edge (after sign-in); controller: the UI and the agents' worker pools; tools: the ops agents. The controller's RBAC covers only `kagent`, `sv-agents` and `sv-mcp` |
| `kagent` | ops agents (k8s, istio, helm, promql, kgateway): SandboxAgents on pool `kagent-ops` | `kagent-ops` | S&V | atenet-router only |
| `ate-system` | Agent Substrate: ate-api, atenet-router, atelet, ate-controller, valkey, rustfs | one SA per component | platform | ate-api and router: kagent controller only; the rest: `ate-system` only |
| `agentgateway-system` | ai-gateway (LLM + MCP) | `ai-gateway` | S&V | the agents' worker pools, by ServiceAccount (models, Cross App Access); the kagent controller (Ledgerline's public catalog) |
| `agentregistry` | agentregistry | `agentregistry` | S&V | edge via ai-gateway (JWT required); kagent controller |
| `sv-agents` | `bob-assistant`: SandboxAgent on pool `bob-assistant` | `bob-assistant` | S&V / Bob | atenet-router only (kagent controller → ate-api → router) |
| `sv-agents` | advisor desk (meeting-prep, market-brief, compliance-check): SandboxAgents on pool `advisor-desk` | `advisor-desk` | S&V | atenet-router only |
| `sv-mcp` | `bob-workspace` (kmcp) | `bob-workspace` | S&V / Bob | **mcp-waypoint only** (the workspace verifies the delegated token's signature too) |
| `sv-mcp` | `mcp-waypoint` (agentgateway as the namespace's waypoint) | `mcp-waypoint` | S&V | every caller of S&V tools, via ztunnel |
| `sv-u4a` | `u4a-adapter` (UMA client: holds Bob's agent's key) | `u4a-adapter` | S&V / Bob | Bob's agent and the kagent controller only |
| `ledgerline-identity` | Keycloak `ledgerline` (ID-JAG receiver) | `keycloak` | Ledgerline | edge (the realm only); `ledgerline-research` (JWKS) |
| `ledgerline` | `ledgerline-research` (kmcp) behind an Istio waypoint | `ledgerline-research` | Ledgerline | edge (Ledgerline token for calls, verified again by the server) |
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

1. **ztunnel (L4, every pod, [Istio ambient](https://docs.solo.io/istio/)).** STRICT mTLS in the party
   namespaces; ALLOW rules name SPIFFE principals from the table above, and
   a workload no ALLOW names takes no connections (`kagent` has an explicit
   `default-deny`).
2. **Waypoints (L7, per party namespace).** `sv-mcp`, `alice`, `meridian`
   and the identity namespaces each run a waypoint (`istio.io/waypoint-for:
   all`). Path- and JWT-based rules live there and bind with `targetRefs`,
   never `selector`: a selector policy with L7 attributes lands on ztunnel and
   becomes a deny.
3. **Gateways (L7, per request).**
   - Edge ([kgateway](https://docs.solo.io/kgateway/)): TLS, one listener per party hostname, and each
     listener only accepts routes from that party's namespaces. On each
     `idp.<party>.lab` it publishes only the party's realm
     (`/realms/<realm>`) and the login pages' assets (`/resources`): the
     admin console, admin API and `master` realm answer 404, and admins use a
     port-forward.
   - ai-gateway ([agentgateway](https://docs.solo.io/agentgateway/), S&V): JWT validation against S&V's Keycloak,
     per-tool MCP authorization in CEL, RFC 8693 token exchange / ID-JAG
     toward tools, provider credentials for LLMs. The model route admits only
     the agents' worker pools, by ServiceAccount. Its listeners take routes
     only from `agentgateway-system`, so no other namespace can publish a
     path on it.
   - meridian gateway (agentgateway, Meridian): ext-auth to uma-pep, which
     enforces Alice's terms (UMA tickets, PoP RPTs, single-use grants).

**Egress.** Every party namespace, and the platform's (`observability`,
`kiali`, `kgateway-system`, `cnpg-system`, `observatory`, `ops-identity`), has
NetworkPolicy `no-internet`: its pods reach the cluster and nothing else
(Prometheus also reaches the nodes' metrics ports). The ways out are the
gateways built for it: ai-gateway (models, Cross App Access) and
`sv-egress/egress-waypoint` (upstream IdPs). Istio ambient doesn't enforce
`outboundTrafficPolicy`, so the CNI (kindnet) does. `ate-system` keeps its
egress: atelet pulls the actors' images itself.

## Identity flows

Delegation at the MCP waypoint (RFC 8693), Cross App Access to Ledgerline
(ID-JAG), and UMA for agents (Bob to Alice), hop by hop:
[IDENTITY-FLOWS.md](IDENTITY-FLOWS.md).

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
where the Observatory's observed edges come from. Kiali (view-only) and
Grafana sit on the edge behind the platform admins' sign-in (realm `ops`), and
take connections only from the edge. They read the
same data.

What the collector receives is what the Observatory shows, so it takes OTLP
only from the ServiceAccounts that report telemetry
(`platform/20-observability/mesh-policy.yaml`). agentgateway exports its
access logs to the collector's Service as a backend (not a URL), so they
leave over the mesh with the gateway's own identity. Prometheus, Tempo and
kube-state-metrics are fenced the same way. Kiali runs in its own `kiali`
namespace, in the mesh: istio-cni excludes `istio-system`, so a workload there
has no identity to be named by. The Observatory derives its map from the cluster's own objects;
see [OBSERVATORY.md](OBSERVATORY.md).

## What the lab doesn't enforce

Known gaps, kept on purpose or pending upstream work:

- **Alice's portal reads her vault directly.** `meridian/alice-vault` admits
  `alice/portal` as well as Meridian's gateway, so the portal shows Alice
  her own holdings without an UMA grant. Agents only reach the vault through
  the gateway and Alice's terms.
- **The UMA adapter signs for any caller it admits.** `sv-u4a/u4a-adapter`
  holds Bob's agent's key and takes calls from Bob's agent's pool and the
  kagent controller (tool listing). It doesn't know which user a call is
  for, and a standing grant from Alice lasts up to 7 days
  (`UMA4A_STANDING_MAX_EXPIRES`).
- **Any namespace can get a lab CA certificate.** The `lab-ca` ClusterIssuer
  signs any `.lab` name for any cert-manager Certificate. In production,
  bind names to namespaces with cert-manager's approver-policy.
- **kagent's controller doesn't verify token signatures.** kagent 0.10 has
  only `trusted-proxy` mode, which reads the user from the forwarded token.
  Its mesh policy admits two callers: the UI, which forwards the access token
  the edge verified, and the agents' worker pools, calling back with the
  token the controller gave their turn. Verifying at a waypoint instead would
  refuse those callbacks once the token's 5 minutes are up mid-turn.
- **The continuity controller's Keycloak account can manage the realm.**
  Pointing the login flow's redirector at a tier is authentication-flow
  config, which Keycloak grants only with `manage-realm`. It is scoped to
  realm `sterling-vance`; the controller reads only its two Secrets.
- **kagent doesn't verify ate-api's TLS certificate** (`ateApiInsecure`). The
  hop runs inside the mesh's mTLS, which authenticates both ends by SPIFFE
  ID; ate-api's own certificate is Substrate's self-issued one.
- **The IdPs are single Keycloaks in dev mode.** `start-dev`, one replica,
  an in-memory store re-imported from the realm file on every start. Fine
  for a lab that rebuilds in minutes; production runs Keycloak with a
  database and several replicas.
- **The password grant is on** for `kagent` (S&V) and `alice-portal`
  (Alice), so the scripted checks can sign in as Bob and Alice. People sign in
  through the browser either way. `LAB_PASSWORD_GRANT=false` in
  `config/lab.env` turns it off. Every realm locks an account for a while
  after 10 wrong passwords.

## Naming and DNS

Every party hostname is `<name>.<party>.lab`, and it resolves the same way
everywhere, so an issuer URL means the same thing to a browser, a CLI and a
pod:

- **Host:** the lab DNS container on `127.0.0.1:15353` answers every
  `*.lab` name with `127.0.0.1`. `make machine-setup` (one-time sudo) points
  the host at it: `/etc/resolver/lab` on macOS, a systemd-resolved drop-in
  routing `~lab` on Linux, or, without systemd-resolved (or on WSL2), a
  marked block in `/etc/hosts` with every hostname the lab publishes.
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
45-identity     S&V Keycloak (patched for ID-JAG), DNS rewrite, client secrets for S&V components
47-continuity   upstream IdP failover for S&V Keycloak (IdentityContinuity, controller, egress waypoint)
50-substrate    Agent Substrate (patched, ate-system in the mesh)
60-kagent       kagent + kmcp, model via ai-gateway, ops agents on Substrate
70-agentregistry agentregistry behind S&V SSO at the edge
80-mesh-policy  S&V mesh baseline (other parties own theirs, in their story)
90-observatory  Observatory, its Keycloak (realm ops), Grafana and Kiali behind it, gateway access logs
95-demos        every story: bob, then bob-to-alice
```
