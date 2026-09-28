# Architecture: who owns what

The lab is four **parties** on one cluster, plus the neutral platform they all
run on. Each party gets its own namespaces, its own service accounts (so its
own SPIFFE identities), its own IdP where the story needs one, and its own
hostnames. Parties never talk pod-to-pod: they meet at the **edge**, the way
separate companies meet on the internet. That is enforced by mesh identity,
not by convention.

| Party | Role in the stories | Domain | Namespaces |
| --- | --- | --- | --- |
| **Platform** | the "cloud": mesh, edge, telemetry, substrate | `ops.lab` | `istio-system` `kgateway-system` `observability` `ate-system` `cert-manager` |
| **Sterling & Vance** | Bob's firm. Runs the Solo AI platform (kagent, agentgateway, agentregistry) for its advisors | `sterling.lab` | `sv-identity` `kagent` `agentgateway-system` `agentregistry` `sv-agents` `sv-mcp` |
| **Alice** | resource owner. Her authorization server, her portal, her IdP | `alice.lab` | `alice` `alice-identity` |
| **Meridian Wealth** | Alice's brokerage. Holds her account, enforces her terms, can never read them | `meridian.lab` | `meridian` |

Bob is a user of Sterling & Vance (realm `sterling-vance` in `sv-identity`).
Alice is a user of her own IdP (realm `alice` in `alice-identity`).

## Workloads and identities

Every workload has its own ServiceAccount. The SPIFFE ID is
`spiffe://cluster.local/ns/<ns>/sa/<sa>`; policies below name these.

| Namespace | Workload | SA | Owner | Reached by |
| --- | --- | --- | --- | --- |
| `kgateway-system` | edge (kgateway/Envoy) | `edge` | platform | laptop (NodePort 30080/30443) |
| `sv-identity` | Keycloak `sterling-vance` | `keycloak` | S&V | edge; S&V gateways/apps (JWKS, token exchange) |
| `sv-identity` | xaa-broker (ID-JAG issuer) | `xaa-broker` | S&V | agentgateway only |
| `kagent` | controller, UI, oauth2-proxy, tools | `kagent-*` | S&V | edge → oauth2-proxy → UI → controller |
| `agentgateway-system` | ai-gateway (LLM + MCP) | `ai-gateway` | S&V | S&V agents; edge (`ai.sterling.lab`, JWT required) |
| `agentregistry` | agentregistry | `agentregistry` | S&V | edge via ai-gateway (JWT required); kagent controller |
| `sv-agents` | Bob's agents (kagent Agents) | one SA per agent | S&V / Bob | kagent controller (A2A) |
| `sv-mcp` | `bob-workspace` (kmcp) | `bob-workspace` | S&V / Bob | **ai-gateway only** (waypoint also checks the delegated token) |
| `sv-mcp` | `u4a-adapter` (agent-shim: Bob's agent key) | `u4a-adapter` | S&V / Bob | ai-gateway only; egress to the edge only |
| `alice-identity` | Keycloak `alice` | `keycloak` | Alice | edge; `alice/uma-as` (JWKS) |
| `alice` | uma-as (Alice's AS) | `uma-as` | Alice | edge (grant surface); `meridian/uma-pep` (protection API); portal (owner API) |
| `alice` | alice-portal | `portal` | Alice | edge |
| `meridian` | meridian gateway (agentgateway) | `meridian` | Meridian | **edge only** |
| `meridian` | uma-pep (ext-auth) | `uma-pep` | Meridian | meridian gateway only |
| `meridian` | alice-vault (kmcp) | `alice-vault` | Meridian, holding Alice's account | meridian gateway; Alice's portal |

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

## Naming and DNS

Every party hostname is `<name>.<party>.lab`, and it resolves the same way
everywhere, so an issuer URL means the same thing to a browser, a CLI and a
pod:

- **Mac:** `/etc/resolver/lab` sends `*.lab` to the lab DNS container on
  `127.0.0.1:15353`, which answers `127.0.0.1`. `make dns-setup` is a
  one-time sudo step.
- **Cluster:** CoreDNS rewrites `*.lab` to the edge Service.
- **TLS:** the lab CA is generated once into `~/.solo-lab/ca` and outlives
  clusters, so you trust it once (`make trust-ca`, one-time sudo). In-cluster
  consumers get it through trust-manager (`lab-ca-bundle` ConfigMap).

Why not `*.localhost`: agentgateway's resolver (hickory, RFC 6761) and newer
Go resolvers answer `*.localhost` with loopback without asking DNS. A
`keycloak.localhost` issuer would then mean "the pod itself" inside the
cluster.

## Install order

```
00-foundation   Gateway API, metrics-server, cert-manager, trust-manager, lab CA, namespaces
10-istio        ambient: base, istiod (HA), cni, ztunnel
20-observability kube-prometheus-stack, Tempo, OTel collector, Kiali
30-kgateway     edge (HA, pinned NodePorts, per-party TLS listeners)
40-agentgateway ai-gateway (HA, mesh-native), LLM backend (make llm)
45-identity     S&V Keycloak, DNS rewrite, SSO for kagent/Grafana/Kiali
50-substrate    Agent Substrate (ate-system, unmeshed)
60-kagent       kagent + kmcp, model via ai-gateway
70-agentregistry agentregistry behind ai-gateway
80-mesh-policy  default-deny, waypoints, per-party ALLOWs
90-solo-mgmt    enterprise only: Solo management UI + relay
demos/          bob (story 1), bob-to-alice (story 2)
```
