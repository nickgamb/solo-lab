# Architecture: who owns what

The lab is four **parties** on one cluster, plus the neutral platform they all
run on. Each party gets its own namespaces, its own service accounts (so its
own SPIFFE identities), its own IdP where the story needs one, and its own
hostnames. Parties never talk pod-to-pod: they meet at the **edge**, the way
separate companies meet on the internet. That is enforced by mesh identity,
not by convention ([zero trust](https://www.solo.io/topics/security-and-compliance/zero-trust)).

| Party | Role in the stories | Domain | Namespaces |
| --- | --- | --- | --- |
| **Platform** | the "cloud": mesh, edge, telemetry, substrate, and the Observatory | `ops.lab` | `istio-system` `kgateway-system` `observability` `kiali` `ate-system` `cert-manager` `cnpg-system` `observatory` `ops-identity` |
| **Sterling & Vance** | Bob's firm. Runs the Solo AI platform (kagent, agentgateway, agentregistry) for its advisors | `sterling.lab` | `sv-identity` `sv-workforce` `sv-contingency` `sv-egress` `kagent` `agentgateway-system` `agentregistry` `sv-agents` `sv-mcp` `sv-u4a` |
| **Alice** | resource owner. Her authorization server, her portal, her IdP | `alice.lab` | `alice` `alice-identity` |
| **Meridian Wealth** | Alice's brokerage. Holds her account, enforces her terms, can never read them | `meridian.lab` | `meridian` |
| **Ledgerline Research** | a SaaS S&V subscribes to (Cross App Access target) | `ledgerline.lab` | `ledgerline` `ledgerline-identity` `ledgerline-egress` |

Bob is an employee of Sterling & Vance. S&V's broker (Keycloak, realm
`sterling-vance` in `sv-identity`) routes his sign-in to the firm's IdPs in
failover order (Auth0, the Keycloak S&V runs itself in `sv-workforce`, its
password-only contingency IdP in `sv-contingency`, Okta, Gluu) and holds only
the profiles those IdPs provision (no employee passwords), plus break-glass
accounts for platform admins. It is never an IdP itself: it is the identity
fabric attached to S&V's gateway, the only issuer anything trusts, so S&V's
IdPs never know each other or the resources (see Identity continuity).
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
| `sv-identity` | Keycloak `sterling-vance` (S&V's broker) | `keycloak` | S&V | edge (the realm only); S&V gateways/apps (JWKS, token exchange) and, on OSS, `bob-workspace` (JWKS); continuity-controller and continuity-sync (admin API) |
| `sv-identity` | `continuity-controller` (2 replicas, leader-elected) | `continuity-controller` | S&V | nobody (calls out only) |
| `sv-identity` | `sterling-vance-profile-sync` (CronJob: the directory sync) | `continuity-sync` | S&V | nobody (calls out only) |
| `sv-identity` | `assurance-gate` (3 replicas: decides each request a gateway policy asks it about) | `assurance-gate` | S&V | the gateways whose policies ask it, on its gRPC port; the Observatory, on its evaluate port |
| `sv-workforce` | Keycloak `workforce` (S&V's own IdP, `login.sterling.lab`; password and a one-time code) | `keycloak` | S&V | edge (browsers, and in-lab callers of `login.sterling.lab`); continuity-sync (admin API) |
| `sv-contingency` | Keycloak `contingency` (S&V's contingency IdP, `login-dr.sterling.lab`; password only) | `keycloak` | S&V | edge only |
| `sv-egress` | `egress-waypoint` (Istio waypoint for external upstream IdPs) | `egress-waypoint` | S&V | the broker's Keycloak, continuity-controller, continuity-sync, observatory |
| `kagent` | controller, UI, tools | `kagent-*` | S&V | UI: edge (after sign-in); controller: the UI, the agents' worker pools, the Observatory, and on Enterprise agentregistry (its kagent runtime); tools: ai-gateway only. The controller's RBAC covers only `kagent`, `sv-agents` and `sv-mcp` |
| `kagent` | ops agents (k8s, istio, helm, promql, kgateway): SandboxAgents on pool `kagent-ops` | `kagent-ops` | S&V | atenet-router only |
| `ate-system` | Agent Substrate: ate-api, atenet-router, atelet, ate-controller, valkey, rustfs | one SA per component | platform | ate-api and router: kagent controller only; the rest: `ate-system` only |
| `agentgateway-system` | ai-gateway (models, every MCP server, Cross App Access) | `ai-gateway` | S&V | the agents' worker pools, by ServiceAccount (models, MCP servers, Cross App Access); the kagent controller (listing S&V's MCP servers' tools); the edge (`llm.sterling.lab`, with an API key); `sv-mcp` (the STS's keys, Enterprise); itself (the Cross App Access token requests) |
| `agentgateway-system` | agentgateway controller (xDS; on Enterprise the STS on port 7777, which mints the tokens MCP servers accept) | `agentgateway` (`enterprise-agentgateway` on Enterprise) | S&V | xDS (9978): the agentgateway proxies; STS: ai-gateway only, and MCP servers read its keys through the ai-gateway (`/sts/jwks.json`, GET, from `sv-mcp`); metrics (9092): Prometheus |
| `agentgateway-system` | Enterprise shared services: ext-auth, rate limiter, WAF, their cache | the chart's | S&V | `agentgateway-system`'s workloads, Alice's waypoint, Ledgerline's `mcp-gateway`, Meridian's gateway, Prometheus |
| `agentgateway-system` | `mcp-guard` (ai-gateway's MCP guardrail: masks account numbers and SSNs in tool results) | `mcp-guard` | S&V | ai-gateway only |
| `agentgateway-system` | `idtoken-exchange` (ai-gateway's ext-auth for Cross App Access: Bob's ID token from S&V's broker) | `idtoken-exchange` | S&V | ai-gateway only |
| `agentregistry` | agentregistry | `agentregistry` | S&V | edge only (after S&V sign-in, kgateway OAuth2); its database: agentregistry only |
| `sv-agents` | `bob-assistant`: SandboxAgent on pool `bob-assistant` | `bob-assistant` | S&V / Bob | atenet-router only (kagent controller → ate-api → router) |
| `sv-agents` | advisor desk (meeting-prep, market-brief, compliance-check): SandboxAgents on pool `advisor-desk` | `advisor-desk` | S&V | atenet-router only |
| `sv-mcp` | `bob-workspace` (kmcp) | `bob-workspace` | S&V / Bob | **ai-gateway only** (the workspace verifies the token the gateway minted for it too) |
| `sv-u4a` | `u4a-adapter` (UMA client: holds Bob's agent's key) | `u4a-adapter` | S&V / Bob | ai-gateway only |
| `ledgerline-identity` | Keycloak `ledgerline` (ID-JAG receiver) | `keycloak` | Ledgerline | edge (the realm only); `ledgerline-research` (JWKS) |
| `ledgerline` | `ledgerline-research` (kmcp) behind `mcp-gateway`, Ledgerline's agentgateway | `ledgerline-research` | Ledgerline | edge (Ledgerline token for calls, verified again by the server) |
| `ledgerline-egress` | `egress-waypoint` (Istio waypoint for the internet hosts Ledgerline reaches: its AS's keys, when that AS is outside the lab) | `egress-waypoint` | Ledgerline | `ledgerline-research` and Ledgerline's Keycloak only |
| `alice-identity` | Keycloak `alice` | `keycloak` | Alice | edge; `alice/uma-as` (JWKS) |
| `alice` | uma-as (Alice's AS) | `uma-as` | Alice | its waypoint only: the edge (grant surface, and Meridian's uma-pep calling the protection API through it) and the portal (owner API) |
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

1. **ztunnel (L4, every pod, [Istio ambient](https://docs.solo.io/istio/)).** STRICT mTLS
   mesh-wide (`platform/80-mesh-policy/mesh.yaml`), PERMISSIVE only on the
   ports callers outside the mesh reach: the edge's listeners (80, 443) and
   the CNPG and Prometheus operator admission webhooks; ALLOW rules name
   SPIFFE principals from the table above, and a workload no ALLOW names
   takes no connections. Every namespace that
   holds an IdP, a session store or a signing key also carries an explicit
   `default-deny` (`sv-identity`, `sv-workforce`, `sv-contingency`, `kagent`,
   `agentgateway-system`, `sv-u4a`,
   `alice`, `alice-identity`, `meridian`, `ledgerline-identity`,
   `ops-identity`, `observatory`, `kiali`, and on Enterprise
   `solo-enterprise`), so a workload added there later
   starts closed.
2. **Waypoints (L7, where a path- or JWT-level rule needs one).**
   - `alice`: an agentgateway waypoint (`enterprise-agentgateway-waypoint`
     on Enterprise), used only by `uma-as`. Its rules are agentgateway
     authorization on the route bound to the Service: the grant surface from
     the edge, the owner API from the portal or the edge. The Service is
     labelled `istio.io/ingress-use-waypoint`, so the edge sends to it by
     Service address, through the waypoint.
   - `meridian`: an Istio waypoint (`waypoint-for: all`), used only by
     `uma-pep`. Meridian's gateway calls uma-pep for ext-auth by pod address,
     which an agentgateway waypoint doesn't take.
   - `ledgerline`: none. Ledgerline fronts its MCP server with its own
     agentgateway (`mcp-gateway`), reached from the edge: per-tool
     authorization on Ledgerline's token, read from each MCP request. A
     gateway rather than a waypoint, because the edge sends to pod
     addresses.
   - `sv-mcp`, `sv-u4a`, kagent's tools: none. S&V's MCP servers sit
     behind the ai-gateway, a path per server, and take connections from it
     alone. A path rather than kagent's proxy setting, because the kagent
     controller lists tools by dialing each server: agents and tool
     discovery go through the same front door.
   - `sv-egress`: `egress-waypoint` for the upstream IdPs' ServiceEntries.
   - The identity namespaces have none; their fences are L4.

   Rules bind to a waypoint with `targetRefs`, never `selector`: a selector
   policy with L7 attributes lands on ztunnel and becomes a deny.
3. **Gateways (L7, per request).**
   - Edge ([kgateway](https://docs.solo.io/kgateway/)): TLS, one listener per party hostname, and each
     listener only accepts routes from that party's namespaces. On each
     `idp.<party>.lab` it publishes only the party's realm
     (`/realms/<realm>`) and the login pages' assets (`/resources`): the
     admin console, admin API and `master` realm answer 404, and admins use a
     port-forward.
   - The assurance gate, at S&V's ai-gateway: in the same policy as a
     route's JWT check, each request asks
     `sv-identity/assurance-gate` (external authorization, fail closed)
     whether the IdP and sign-in behind the token meet the assurance rule the
     policy names (a `WorkloadProfile`, or the chain's default rule). It
     decides from the verified claims and the rules it watches, with no call
     to an IdP, and refuses what can't meet them
     (IDENTITY-CONTINUITY.md#assurance-rules).
   - ai-gateway ([agentgateway](https://docs.solo.io/agentgateway/), S&V): the front door for every MCP
     server, a path each (`/mcp/bob-workspace`, `/mcp/kagent-tools`,
     `/mcp/alice-vault`, `/xaa/ledgerline/mcp`), and for AI clients outside
     the mesh, through the edge at `mcp.sterling.lab` (MCP authorization:
     a 401 that sends the client to sign in at S&V's primary IdP through the
     fabric, as the one client S&V registered for them). It finds each server by
     its Service's `lab.solo.io/mcp` label and reaches it over the mesh as
     itself. JWT validation against
     S&V's broker, the caller's workload and groups, per-tool MCP
     authorization in CEL (an allowlist on every path), a guardrail on tool
     results, and a short-lived token of its own for each MCP server, minted
     per call (agentgateway's STS on Enterprise,
     an RFC 8693 exchange at S&V's broker on OSS), or a fresh ID-JAG per
     call toward Ledgerline; nothing minted is cached, and the user's token
     never reaches a tool. Provider credentials
     for LLMs. The model route admits only
     the agents' worker pools, by ServiceAccount; the user's token the
     agents send there names whose turn it is and stops at the gateway, so
     no model provider sees it. Developers reach the same models through
     the edge at `llm.sterling.lab` (`/v1` only, with an API key). Every model call meets
     prompt guards (an attempt to override the agent's instructions is
     refused; card and social security numbers are masked before the prompt
     leaves and in the answer), and with `LLM_FALLBACK` a provider that
     fails is taken out of rotation while a second one serves. Its listeners take routes
     only from `agentgateway-system`, so no other namespace can publish a
     path on it.
   - meridian gateway (agentgateway, Meridian): ext-auth to uma-pep, which
     enforces Alice's terms (UMA tickets, PoP RPTs, single-use grants).
4. **The API server (admission).** Pod Security Admission enforces
   `baseline` in every lab namespace and warns at `restricted`; only
   `ate-system`, `kagent` and `sv-agents` (Agent Substrate's gVisor worker
   pools) and `observability` (node exporters) are `privileged`. Two
   ValidatingAdmissionPolicies fence the continuity controller's one write
   outside its CRD: the CronJob it creates for the directory sync must run as
   `continuity-sync` with no Secret, projected or host volume and no Secret
   in its environment, and the sync may change only `status.sync` on the
   IdentityContinuity (the active IdP stays the controller's). Two more keep
   assurance rules honest: only the controller writes a rule's status,
   and no policy that asks the assurance gate may fail open. Confidential
   Keycloak clients have full scope off: a token carries only the claims its
   mappers add, and an admin client's token only the `realm-management` roles
   its scope mapping names (`clientScopeMappings` in the realm file).

**Egress.** Every party namespace, and the platform's (`observability`,
`kiali`, `kgateway-system`, `cnpg-system`, `observatory`, `ops-identity`, and
on Enterprise `solo-enterprise`), has
NetworkPolicy `no-internet`: its pods reach the cluster and nothing else
(Prometheus also reaches the nodes' metrics ports). In `agentgateway-system`,
where the ai-gateway reaches the model providers, `mcp-guard` and
`idtoken-exchange` have their own (`no-internet-<app>`). The ways out are the
gateways built for it: ai-gateway (models, Cross App Access),
`sv-egress/egress-waypoint` (S&V's upstream IdPs) and
`ledgerline-egress/egress-waypoint` (the keys of Ledgerline's AS outside the lab). Istio ambient doesn't enforce
`outboundTrafficPolicy`, so the CNI (kindnet) does. `ate-system` keeps its
egress: atelet pulls the actors' images itself.

## Identity flows

Delegation at the firm's gateway, Cross App Access to Ledgerline
(ID-JAG), and UMA for agents (Bob to Alice), hop by hop:
[IDENTITY-FLOWS.md](IDENTITY-FLOWS.md).

## Identity continuity

`IdentityContinuity` (`kubectl get idc -n sv-identity`) is an ordered chain
of S&V's IdPs (Okta, Auth0, Gluu, the Keycloak S&V runs itself in
`sv-workforce`, or its contingency IdP in `sv-contingency`) for S&V's broker,
ending in the broker's break-glass accounts for platform admins. The continuity controller probes every IdP
and points the broker's login at the first healthy one; the broker stays the
issuer everything trusts and maps every IdP into one profile, and brokered
users are linked to their S&V user by verified email, so `sub` never changes.
External upstreams are reached through `sv-egress/egress-waypoint` (one
ServiceEntry per IdP, exported to every S&V namespace that calls them),
which is also where an outage is simulated: a DENY policy on that
ServiceEntry. Cross App Access calls no upstream: the broker vouches for
S&V's users, and the assurance gate refuses a session from an IdP that is no
longer active.

Failover keeps sign-in up; assurance rules keep it from lowering what a
sign-in proves. The broker carries each upstream's `acr`, `amr` and
`auth_time` on the session, each IdP declares what they're worth (NIST
800-63B levels), the chain's `assurancePolicy` is the default rule, and a
rule (a `WorkloadProfile`, `kubectl get wlp -n sv-identity`) says where a
group of workloads differs: a minimum assurance, which IdPs may vouch,
break-glass, sessions; enforced, report-only or off. The assurance gate
enforces them at the gateway policies that ask it, answers what they decide
on its evaluate port (the Observatory's Assurance rules), and each rule's
status says, as the chain moves,
whether they can be met (`Available`, `Degraded`, `FailedClosed`). The controller also checks every
IdP's acceptance of the broker's registration (callback, client authentication,
PKCE, scopes, claims, assurance values). Details, the API and Auth0 setup:
[IDENTITY-CONTINUITY.md](IDENTITY-CONTINUITY.md).

## Scaling the design

The lab is one cluster. In production the same shapes run across clusters,
and Solo Enterprise for Istio adds what that needs:

- **Global services.** The broker (Keycloak), the egress gateway and the
  apps run in each cluster; a service marked global is reachable by one
  name everywhere, so HTTPRoutes and policies are written once and applied
  the same way in every cluster.
- **Cross-cluster failover.** After sign-in at the IdP, requests fail over
  to the same service in another cluster when the local one is unhealthy,
  with no change to routes or clients. IdP continuity (which IdP signs
  people in) and service failover (where the service runs) are independent
  and compose.
- **Multi-cluster graph.** One view of every cluster's services, the
  traffic between them, and where failover sent it.

Not built here: it needs a second cluster and the enterprise edition.

## Observability and the Observatory

Every component sends OTLP to one collector (`observability/otel-collector`):
traces to Tempo, metrics to Prometheus, gateway access logs to the
Observatory (the edge, ai-gateway, Meridian's and Ledgerline's gateways). Prometheus also scrapes ztunnel, waypoints and gateways, which is
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
  holds Bob's agent's key and takes calls only through the ai-gateway, which
  admits Bob's agent's pool acting for an advisor and the kagent controller
  (tool listing). The advisor's token stops at the gateway, so the adapter
  doesn't know which user a call is
  for, and a standing grant from Alice lasts up to 7 days
  (`UMA4A_STANDING_MAX_EXPIRES`).
- **Any namespace can get a lab CA certificate.** The `lab-ca` ClusterIssuer
  signs any `.lab` name for any cert-manager Certificate. In production,
  bind names to namespaces with cert-manager's approver-policy.
- **OSS kagent's controller doesn't verify token signatures.** kagent 0.10 has
  only `trusted-proxy` mode, which reads the user from the forwarded token
  (kagent-enterprise verifies it against S&V's broker, ENTERPRISE.md).
  Its mesh policy admits two callers: the UI, which forwards the access token
  the edge verified, and the agents' worker pools, calling back with the
  token the controller gave their turn. Verifying at a waypoint instead would
  refuse those callbacks once the token's 5 minutes are up mid-turn.
- **The continuity controller's Keycloak account can manage the realm.**
  Pointing the login flow's redirector at an IdP is authentication-flow
  config, which Keycloak grants only with `manage-realm`. It is scoped to
  realm `sterling-vance`; the controller reads only the Secrets named in
  Role `continuity-controller-secrets`.
- **kagent doesn't verify ate-api's TLS certificate** (`ateApiInsecure`). The
  hop runs inside the mesh's mTLS, which authenticates both ends by SPIFFE
  ID; ate-api's own certificate is Substrate's self-issued one.
- **A few ports take plaintext.** STRICT mTLS is mesh-wide, but the load
  balancer in front of the edge and the API server calling admission
  webhooks aren't in the mesh, so the edge's listeners (80, 443) and the
  CNPG and Prometheus operator webhook ports are PERMISSIVE.
- **The agentgateway controller's xDS port (9978) takes plaintext, from any
  caller.** Its proxies run outside the mesh, terminate HBONE themselves and
  fetch their configuration before they hold a mesh identity, so they can't
  reach it over mesh mTLS. The controller's other ports stay STRICT, each
  open to its one caller (the STS to the ai-gateway, metrics to
  Prometheus).
- **Platform admins can redirect traffic.** The Observatory's admins may
  change gateway policies and backends, which can name Secrets in their own
  namespace (an LLM provider's key, an OAuth client's secret): pointing a
  backend elsewhere sends that credential with it. They can't read Secrets,
  change workloads, RBAC or admission (OBSERVATORY.md).
- **Platform UIs authorize at the edge only.** Grafana and Kiali accept
  anyone the edge's OAuth2 filter signs in from the ops realm; they don't
  check a group themselves (Kiali is view-only).
- **Telemetry is trusted from the mesh.** The Observatory's traffic feed is
  the gateways' access logs over OTLP; any principal `collector-callers`
  admits could write to it. The ALLOW list is the integrity control.
- **One trust domain, `cluster.local`.** Every policy names
  `spiffe://cluster.local/...`; a second cluster needs its own trust domain
  and the policies parameterized on it.
- **The lab's TLD is `.lab`,** which isn't reserved. `.test` or `.internal`
  would be; the hostnames stay because the docs and screenshots use them.
- **The IdPs are single Keycloaks in dev mode.** `start-dev`, one replica,
  an in-memory store re-imported from the realm file on every start. Fine
  for a lab that rebuilds in minutes; production runs Keycloak with a
  database and several replicas.
- **The broker is a single point of failure.** Identity continuity removes
  the upstream IdP as one; the broker every workload trusts is still one
  Keycloak. While it is down, tokens already issued keep validating (every
  gateway caches its keys), but nobody signs in, refreshes or exchanges a
  token, and in dev mode a restart ends every session. Production runs the
  broker highly available: several replicas on a shared database, in more
  than one zone or cluster (Scaling the design). The assurance gate is built
  not to add another: no per-request dependency on the broker or an IdP,
  replicated, and fail closed.
- **Console sign-ins aren't gated by assurance rules.** The kagent and
  agentregistry consoles sign people in at the edge (kgateway OAuth2), which
  doesn't ask the assurance gate; the Observatory lists them as relying on
  the broker without a rule. What the agents do on the user's behalf is
  gated where it lands (the workspace, Cross App Access).
- **The password grant is on** for Alice's `alice-portal`, so the scripted
  checks can sign her in. The checks sign Bob in through the browser flow
  (`sso_token` in `scripts/lib.sh`: kagent's SSO, through the broker to S&V's
  own Keycloak, which they make the active IdP for their run).
  `LAB_PASSWORD_GRANT=false` in `config/lab.env` turns the grant off. Every
  realm locks an account for a while after 10 wrong passwords.

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
| `tools/keycloak-idjag` 0001 | Keycloak 26.7.5 + PR #49998 (ID-JAG issuing), backported; plus identity continuity's IdP mapper (`session-claims/`, a provider, not a patch) | keycloak/keycloak#49998 |
| `tools/keycloak-idjag` 0002 | an ID-JAG lives no longer than its audience's client allows (how long the issuer vouches to that authorization server) | keycloak/keycloak (PR to open) |
| `tools/kagent` 0001 | Go ADK: SandboxAgents call the controller back with the caller's credential (they have no ServiceAccount token). Both editions run this ADK | kagent-dev/kagent (PR to open) |
| `tools/kagent` 0002 | OSS controller: a SandboxAgent turn sent as the last one closes (HITL approval) no longer races its suspend | kagent-dev/kagent (PR to open) |
| `tools/substrate-mesh` 0001 | Substrate 0.0.9: WorkerPool pod identity (`serviceAccountName`, labels, annotations) | kagent-dev/substrate (PR to open) |
| `tools/substrate-mesh` 0002 | a pool with its own ServiceAccount only runs its namespace's actors | kagent-dev/substrate (PR to open) |
| `tools/substrate-mesh` 0003 | ateom: inbound relayed from a local socket, worker-local traffic pinned, so actors work under in-pod mesh capture | kagent-dev/substrate (PR to open) |

## Install order

```
00-foundation   Gateway API, metrics-server, cert-manager, trust-manager, lab CA, namespaces (party and Pod Security labels), CoreDNS *.lab -> edge
10-istio        ambient: base, istiod (HA), cni, ztunnel
20-observability kube-prometheus-stack, Tempo, OTel collector, Kiali
30-kgateway     edge (HA, pinned NodePorts, per-party TLS listeners)
40-agentgateway ai-gateway (HA, mesh-native), LLM backend (make llm), on Enterprise the STS for MCP servers' tokens
45-identity     S&V's mesh baseline, S&V's broker (Keycloak, idp.sterling.lab, patched for ID-JAG, with the session-claims mapper), S&V's own Keycloak (login.sterling.lab, realm workforce), its contingency IdP (login-dr.sterling.lab), client secrets for S&V components
47-continuity   IdentityContinuity and WorkloadProfile CRDs and controller, the directory sync, the assurance gate (admission policies fence them), S&V egress waypoint
50-substrate    Agent Substrate (patched, ate-system in the mesh)
60-kagent       kagent + kmcp, model via ai-gateway, ops agents on Substrate, their tools (kagent-tools, its own release) behind ai-gateway
70-agentregistry agentregistry behind S&V SSO at the edge
80-mesh-policy  S&V mesh baseline, re-applied (other parties own theirs, in their story), STRICT mTLS mesh-wide
90-observatory  Observatory, its Keycloak (realm ops), Grafana and Kiali behind it, gateway access logs
95-demos        every story: bob, then bob-to-alice
```
