# Identity flows

The three ways an agent gets access in this lab, from the token in the
browser to the call the tool receives. Each section covers who takes part,
the token at each hop, the policy that decides, the patches it needs, and
what the checks prove.

| Flow | Question it answers | Demo card | Checks |
| --- | --- | --- | --- |
| [Delegation](#1-delegation-rfc-8693-at-the-mcp-waypoint) | how does Bob's agent act for Bob on the firm's own tools? | [Bob](cards/bob.html) | `make bob-verify` |
| [Cross App Access](#2-cross-app-access-id-jag-to-a-saas) | how does it reach a SaaS the firm subscribes to, as Bob? | [Bob](cards/bob.html) | `make bob-verify` |
| [UMA for agents](#3-uma-for-agents-bob-to-alice) | how does it reach data owned by someone outside the firm, on their terms? | [Bob to Alice](cards/bob-to-alice.html) | `make alice-verify` |

In every flow two identities are checked: the **workload** (the agent's SPIFFE
ID, from the mesh) and the **user** (Bob, from a token). Neither alone is enough.
The agent holds no credential of its own: no model key, no tool token, no
cross-company token.

## Where the user's tokens come from

1. Bob opens `https://kagent.sterling.lab`. The kgateway edge runs the OIDC
   code flow against S&V's broker (realm `sterling-vance`, client `kagent`,
   `platform/60-kagent/edge-sso.yaml`) and keeps two cookies:
   - `BearerToken`: Bob's **access token**, audiences `ai-gateway`,
     `mcp-waypoint` and `xaa-egress`, plus claim `idp` (the IdP of the
     session). The edge forwards it to kagent as `Authorization`.
   - `IdToken`: Bob's **ID token**, issued to `kagent`. It stays at the edge.
2. The kagent controller passes `Authorization` to the agent on each A2A turn,
   and nothing else of Bob's.
   - OSS kagent runs in trusted-proxy mode: it takes the user from the
     forwarded token without checking its signature, so the mesh admits only
     the UI (which forwards what the edge verified) and the agents' worker
     pools ([ARCHITECTURE.md](ARCHITECTURE.md#what-the-lab-doesnt-enforce)).
   - kagent-enterprise verifies the token against S&V's broker itself
     ([ENTERPRISE.md](ENTERPRISE.md)).
3. The agent (`sv-agents/bob-assistant`, a SandboxAgent on Agent Substrate)
   forwards `Authorization` on every tool call (`KAGENT_PROPAGATE_TOKEN`).

The agent never holds Bob's ID token. The one flow that needs it, Cross App
Access (section 2), has the egress gateway get it from the enterprise IdP
that vouches for Bob. The edge forwards the access token because Keycloak's
standard token exchange only accepts an access token as its subject.

## 1. Delegation (RFC 8693) at the MCP waypoint

Bob's workspace (`sv-mcp/bob-workspace`, a kmcp server) never sees Bob's
token. The waypoint in front of it verifies Bob and the agent, then swaps
Bob's token for one only the workspace accepts, for two minutes.

In RFC 8693's terms the swap is *impersonation*, not delegation: the
waypoint sends only a subject token (Bob's), so the new token names Bob and
has no `act` claim for the agent. Which agent is calling is established by
the mesh (the worker pool's SPIFFE ID, checked at the waypoint), not
recorded in the token. Keycloak's standard token exchange doesn't take an
actor token yet; with one, the token would carry `act: {sub: <agent>}`.

```mermaid
sequenceDiagram
  participant A as Bob's agent (sv-agents)
  participant W as mcp-waypoint (agentgateway, sv-mcp)
  participant K as S&V broker (Keycloak)
  participant T as bob-workspace
  A->>W: tools/call, Authorization: Bob's access token (HBONE, SPIFFE ID of the agent)
  W->>W: verify JWT (aud mcp-waypoint), caller is an agent's pool, Bob in advisors, tool allowed
  W->>K: token exchange (RFC 8693) as client mcp-waypoint, subject = Bob's token
  K-->>W: token for Bob, aud bob-workspace, 120 s
  W->>T: tools/call, Authorization: the exchanged token (Bob's own token removed)
  T->>K: JWKS (cached): verify the exchanged token's signature, issuer, audience, expiry
  T-->>A: result, acting for bob
```

- **Path.** The agent calls the tool's own address,
  `bob-workspace-mcp.sv-mcp:3000`. Ambient mesh sends every call to that
  Service through the namespace's waypoint (an agentgateway), so no route
  skips it. Pods behind it take connections from the waypoint only.
- **User lane** (a bearer token is present), all in
  `demos/bob/manifests/20-waypoint.yaml`:
  - JWT, strict: issuer `https://idp.sterling.lab/realms/sterling-vance`,
    audience `mcp-waypoint`.
  - Authorization: the caller is an agent's worker pool, named by
    ServiceAccount (`bob-assistant` or `advisor-desk` in `sv-agents`), and
    `"advisors" in jwt.groups`. Another workload in `sv-agents` holding
    Bob's token is refused.
  - Per-tool CEL: advisors get `whoami`, `list_clients`, `get_client`,
    `get_meeting_notes`, `log_followup`. `export_book` needs group
    `compliance` (no user in the lab is in it; add one to group `compliance`
    at the broker, realm `sterling-vance`, to try it), and a tool the caller may not use is removed from
    `tools/list`, so the agent is never shown it.
  - Backend auth: `oauthTokenExchange` against S&V's broker as client
    `mcp-waypoint`, with subject `jwt.rawToken.unredacted()`, audience
    `bob-workspace`. The client's token lifespan is 120 s.
  - The workspace verifies that token itself (signature against the broker's JWKS,
    issuer, audience `bob-workspace`, expiry) before any tool runs, so a
    forged or replayed token is refused even if something reached the pod.
  - Tool output: every `tools/call` result goes through `mcp-guard`
    (`apps/mcp-guard`, an agentgateway MCP guardrail, ExtMCP over gRPC)
    before the agent sees it. Account numbers (runs of 8 to 17 digits) and
    SSNs in the result's text and `structuredContent` are masked to their
    last four (`••••8265`). The guard logs the tool and how many values it
    masked, never the values. `failureMode: FailClosed` with a 5 s deadline:
    no answer from the guard, no result. Only the waypoint can reach it
    (`demos/bob/manifests/25-mcp-guard.yaml`).
- **Discovery lane** (no token): only the kagent controller's SPIFFE ID, so
  the UI can list tools. Same tool filter, no exchange, and the route strips
  `Authorization` and `X-Id-Token`, so nothing that looks like a credential
  reaches the workspace on it. A tool call here has no delegated token and
  the workspace refuses it.
- **Writes wait for Bob.** `log_followup` is in the agent's
  `requireApproval`, so the agent pauses for his approval in the chat. The
  entry records the user (`bob`) and the party that carried it out
  (`mcp-waypoint`).

Checks (`make bob-verify`, from real pods with their own identities):

| Case | Expected |
| --- | --- |
| an S&V agent workload with Bob's token lists tools, calls `whoami`, `list_clients` | allowed; `acting_for: bob`, `audience: bob-workspace` |
| `get_client` for a client with an account number | the record, account number masked to its last four by the waypoint's guardrail |
| `export_book` as an advisor | not in the list; refused |
| an S&V agent workload with no user token | refused (discovery lane is controller-only) |
| Bob's token from another namespace (`observability`) | refused |
| Bob's token from a workload in `sv-agents` that isn't an agent's pool | refused |
| Bob's token sent straight to a workspace pod IP | refused (the pod only accepts the waypoint) |

## 2. Cross App Access (ID-JAG) to a SaaS

Ledgerline Research is a SaaS S&V subscribes to, a separate company with its
own authorization server. Bob's agent uses Ledgerline as Bob, with no consent
screen and no shared credential. S&V's enterprise IdP vouches for Bob to
Ledgerline for one approved connection, and Ledgerline issues its own
short-lived token. Every exchange happens at the firm's egress, so the agent
holds only Bob's S&V access token: never an ID token, the ID-JAG or
Ledgerline's token.

Two settings in `config/lab.env` (`scripts/idp.sh`):

| Setting | Values | Decides |
| --- | --- | --- |
| `ENTERPRISE_IDP` | `auth0,keycloak` (default), `okta,auth0,keycloak`, `gluu,keycloak`, ...; any order | S&V's IdPs, which sign Bob in, in failover order ([IDENTITY-CONTINUITY.md](IDENTITY-CONTINUITY.md)), and who vouches for him; `keycloak` is the Keycloak S&V runs itself (`https://login.sterling.lab`) |
| `RESOURCE_AS` | `keycloak` (default), `gluu` with `RESOURCE_AS_ISSUER` | Ledgerline's authorization server |

Who vouches is decided by Bob's session. S&V's broker records the IdP a
session came from, and kagent's access token carries it (`idp`).

| Bob's session | Vouches |
| --- | --- |
| an IdP that issues ID-JAGs (Gluu, S&V's own Keycloak), while it is S&V's active IdP | that IdP |
| an IdP that doesn't (Okta, Auth0), while it is active | S&V's broker, for that session |
| the broker's break-glass accounts (platform admins) | S&V's broker |
| an IdP that is no longer active (failed over, drained) | nobody: refused until Bob signs in again |

Failover is the continuity controller's decision alone (`status.active`).
An upstream that isn't active is disabled at the broker (no sign-in through
it, even by hint) and never called; ID tokens cached from it are dropped.
Every S&V call to an upstream leaves through `sv-egress`, so cutting an
upstream there cuts all of them. An upstream that refuses (revoked,
disabled) refuses the call. Ledgerline tokens already issued live out their
five minutes in the gateway's cache.

```mermaid
sequenceDiagram
  participant A as Bob's agent
  participant G as ai-gateway (S&V egress)
  participant X as idtoken-exchange (ext_proc)
  participant K as S&V broker (Keycloak)
  participant R as xaa-relay
  participant E as Bob's IdP (S&V's own Keycloak, Gluu)
  participant L as Ledgerline AS (Keycloak or Gluu)
  participant M as ledgerline-research (behind Ledgerline's MCP gateway)
  A->>G: tools/call /xaa/ledgerline/mcp, Authorization: Bob's access token
  G->>G: verify JWT (aud ai-gateway), caller is an agent's pool, Bob in advisors
  G->>X: request headers, verified token as metadata
  X->>K: as xaa-egress (its key): Bob's upstream access token (Identity Brokering API v2, renewed by the broker)
  X->>E: token exchange, as S&V's client there (the egress's key): Bob's ID token
  X-->>G: x-id-token (replaces any the caller sent)
  G->>R: token exchange: subject = Bob's ID token, requested type ID-JAG, audience = Ledgerline's AS
  R->>R: only this exchange, this audience, these scopes
  R->>E: as S&V's client at the issuer of the ID token
  E-->>R: ID-JAG for Bob, for this connection only
  R->>R: check typ, signature, iss, aud, sub, client_id, exp, lifetime
  R-->>G: ID-JAG
  G->>R: JWT authorization grant (RFC 7523), scope research:read, private_key_jwt as sterling-vance-kagent
  R->>L: relayed as signed
  L-->>G: Ledgerline access token for Ledgerline's Bob, aud ledgerline-research, scope research:read
  G->>M: via the edge (TLS), Authorization: Ledgerline's token
  M-->>A: result
```

With S&V's broker vouching, idtoken-exchange gets Bob's ID token from it by
RFC 8693 instead (`subject_token` = the access token, `requested_token_type`
= ID token, as `kagent`).

- **Requesting side** (`demos/bob/manifests/40-xaa-ledgerline.yaml`):
  - Route `xaa-ledgerline` on ai-gateway, matched only with a bearer token.
  - JWT, strict, audience `ai-gateway`. Authorization: the caller is an
    agent's worker pool (by ServiceAccount) and `"advisors" in jwt.groups`.
  - External processor `idtoken-exchange` (`apps/idtoken-exchange`, gRPC
    ext_proc): request headers only, fail closed, the verified token passed
    as metadata (`jwt.rawToken`), never read from a header the caller sets.
    It sets `x-id-token`, replacing any value the caller sent, or answers 403
    (refused) or 503. Only ai-gateway may call it (mesh policy).
    - Why not the agent or the gateway's own exchange: kagent passes agents
      the access token, not the ID token, on either edition, and
      agentgateway's `oauthTokenExchange` requires `token_type: Bearer` in
      the response, while RFC 8693 (2.2.1) has the IdP return `N_A` for an
      ID token.
    - Upstream: S&V's broker keeps the user's tokens from their sign-in
      there (`storeTokens`, no offline access: they end with the user's
      session at the IdP). Only client `xaa-egress` may read them, and it
      gets the upstream access token alone, renewed by the broker; the
      refresh token never leaves the broker (Identity Brokering API v2:
      `external.token.idp` lists the IdPs that vouch; the client
      authenticates with the egress's key and must be in the user's token
      audience). The IdP then exchanges that access token for Bob's ID
      token there (RFC 8693, as S&V's client, the egress's key), on each
      use, at most once a minute per access token, so the IdP decides each
      time. Nothing is kept between replicas.
    - It reads the active IdP from IdentityContinuity
      `sv-identity/sterling-vance` (Role: `get` on that object only), isn't
      ready until it knows it, and stops answering if it can't read it for
      30 s.
  - Backend auth `crossAppAccess`: subject from header `x-id-token`, type ID
    token; audience Ledgerline's AS issuer; scopes
    `xaa-ledgerline research:read` for the ID-JAG and `research:read` for
    Ledgerline's token. Both token endpoints are `xaa-relay`
    (`apps/xaa-relay`), which only ai-gateway may call:
    - `/op/token` admits only this connection's request (token exchange,
      requested type ID-JAG, subject type ID token, Ledgerline's AS as
      audience, allowed scopes) and sends it to the IdP that issued the
      subject token, as S&V's client there. The gateway names the requesting
      app (`kagent`) and holds no IdP credential. The ID-JAG is checked before
      the gateway uses it: `typ` `oauth-id-jag+jwt`, signature against that
      IdP's JWKS, `iss`, `aud` = Ledgerline's AS, `sub` = the subject token's,
      `client_id` = `sterling-vance-kagent`, `exp`, at most 300 s.
    - `/ras/token` admits only a JWT authorization grant and passes it to
      Ledgerline's AS as sent: S&V authenticates with `private_key_jwt`
      (client `sterling-vance-kagent`, assertion audience the AS's issuer,
      key `.lab/keys/sv-xaa-client.key`).
    - Both legs are logged with parameters and claims, credentials redacted
      (`make xaa-logs`).
  - The agent's tool (`RemoteMCPServer ledgerline-research`) points at
    `ai-gateway/xaa/ledgerline/mcp`.
- **S&V's clients and keys** (realm `sterling-vance`):

  | Client | Holds | May |
  | --- | --- | --- |
  | `kagent` (secret: the edge and the egress, one application) | — | SSO for kagent; RFC 8693 (access token to ID token); request the ID-JAG with optional scope `xaa-ledgerline`. Keycloak issues an ID-JAG only for an ID token of the app the user signed into. No full scope |
  | `xaa-egress` (the egress's key, `private_key_jwt`) | — | read users' stored upstream tokens, for the vouching upstreams only. Nothing else |
  | broker key (PS256 realm key, client assertions only) | Keycloak | authenticate S&V's broker to upstream IdPs |
  | egress key | idtoken-exchange, xaa-relay | authenticate S&V to upstream IdPs (renewal, ID-JAG) |

  Scope `xaa-ledgerline` adds `client_id=sterling-vance-kagent` and
  Ledgerline's AS issuer as audience: the connection exists because the firm
  approved this scope for this client. `make xaa-keys` writes the public
  keys other parties register.
- **Resource AS**, `RESOURCE_AS=keycloak` (Ledgerline Keycloak, realm
  `ledgerline`, feature `identity-assertion-jwt`,
  `demos/bob/ledgerline/realm-ledgerline.json`):
  - An identity provider per IdP that vouches for S&V's users:
    `sterling-vance` (S&V's broker) and `sterling-vance-<idp>` for each IdP
    in the chain that issues ID-JAGs (`sterling-vance-keycloak`,
    `sterling-vance-gluu`). Each accepts ID-JAGs (assertion reuse off, at most 300 s) and is
    Ledgerline's SSO for S&V (client `ledgerline` there, `private_key_jwt`
    with Ledgerline's own PS256 key, PKCE). Each is trusted for S&V's domain
    only (`email` must end in `@sterling.lab`).
  - Bob's Ledgerline account is linked to his `sub` at each IdP by his first
    sign-in to Ledgerline through it (flow `enterprise first sign-in`: a new
    account named by its verified email, or the existing seat with that
    email). The lab provisions his link to S&V's broker; his link to each
    vouching IdP comes from his first sign-in to Ledgerline through it
    (`make bob-verify` does this).
  - Client `sterling-vance-kagent` may use the JWT authorization grant with
    those IdPs, authenticates with `private_key_jwt` (S&V's certificate and
    `kid`, no secret), and gets scope `research:read` (audience
    `ledgerline-research`) only when it asks. No full scope.
  - Ledgerline's Keycloak reaches an upstream's JWKS through Ledgerline's
    egress waypoint (`ledgerline-egress`, one ServiceEntry per host).
- **Resource AS**, `RESOURCE_AS=gluu`: Gluu at `RESOURCE_AS_ISSUER` trusts
  S&V's IdPs and registers S&V's client key (`make xaa-keys`). It must be a
  separate deployment from any Gluu in `ENTERPRISE_IDP`: an IdP can't vouch
  for Bob to itself, and the install refuses it.
- **Resource server** (`demos/bob/ledgerline/research.yaml`): Ledgerline's own
  agentgateway (`mcp-gateway`) in front of its MCP server, reached only from
  the edge. It reads each MCP request and decides per tool on a token from
  Ledgerline's AS (audience `ledgerline-research`): anyone may list the
  catalog, `sector_outlook` and `research_note` need scope `research:read`,
  `account_info` a signed-in subject. A method header that disagrees with
  the request is refused. The server verifies the token again (signature, issuer,
  audience, scope, a registered client) before any tool runs, and records
  any ID token that reaches it.
- **Discovery lane:** the kagent controller's SPIFFE ID may list Ledgerline's
  public catalog through `/xaa/ledgerline` without a user. The route strips
  `Authorization` and `X-Id-Token`, and it can never get a Ledgerline token.

Checks (`make bob-verify`). It signs Bob in through S&V's own Keycloak,
draining the IdPs ahead of it for its run (the Observatory shows the
transitions); the chain is restored on exit.

| Case | Expected |
| --- | --- |
| `account_info` with Bob's access token from an S&V agent workload | Ledgerline's own account for Bob |
| the ID-JAG xaa-relay accepted | from the IdP that vouches for Bob's session, `typ` `oauth-id-jag+jwt`, `aud` Ledgerline's AS |
| `sector_outlook` through XAA | result |
| another user's ID token in `x-id-token` | replaced: still Bob's Ledgerline account |
| what Ledgerline received | never an ID token |
| right token, wrong workload (`observability`) | refused |
| an agent workload calling `idtoken-exchange` directly | refused by the mesh |
| Bob's S&V token sent straight to `mcp.ledgerline.lab` | refused by Ledgerline |
| Ledgerline's tool list, no token | listed (the catalog is public) |
| a research call claiming to be `tools/list` in the `mcp-method` header | refused at Ledgerline's MCP gateway (header and body disagree) |
| a research call with no Ledgerline token | refused at Ledgerline's MCP gateway |
| Bob asks his agent, in chat, which Ledgerline account he's using | Ledgerline's own account for Bob |

## 3. UMA for agents (Bob to Alice)

Alice's portfolio is held by Meridian Wealth, her brokerage, and it's her
data. Bob's firm doesn't hold it, and Meridian can't decide who reads it.
Alice's own authorization server (UMA 2.0) holds her terms. Meridian's
gateway asks that server on every call, and Bob's agent has to earn a grant
on her terms, bound to a key only it holds.

The protocol profile, its services and their tests are in
[uma4agents](https://github.com/nickgamb/uma4agents) (`docs/PROTOCOL.md`,
`docs/ARCHITECTURE.md`). This lab runs those services as separate parties
behind the mesh, built from that repository at `U4A_TAG`
(`demos/bob-to-alice/install.sh`).

```mermaid
sequenceDiagram
  participant A as Bob's agent
  participant U as u4a-adapter (S&V, holds the agent's key)
  participant M as Meridian gateway + uma-pep
  participant AS as Alice's AS (uma-as)
  participant P as Alice (portal)
  A->>U: tool call (holdings)
  U->>M: request, signed with the agent key (RFC 9421)
  M->>AS: register permission (PAT)
  M-->>U: 401, UMA challenge: permission ticket + Alice's AS
  U->>AS: ticket, agent key, purpose
  AS-->>U: Alice's terms
  U->>AS: signed agreement
  AS->>P: pending: new agent, tier 1
  P-->>AS: Alice approves
  AS-->>U: RPT bound to the agent key
  U->>M: request again, RPT + signature
  M->>AS: introspect (and consume, if single-use)
  M-->>A: result from alice-vault
```

- **Parties** (`demos/bob-to-alice`):
  - S&V: `sv-u4a/u4a-adapter`, the UMA client. It holds the agent's Ed25519
    key (a fresh one per pod, so the agent is pseudonymous to Alice) and runs
    ticket, terms, agreement, and RPT for the agent. Only Bob's agent and the
    kagent controller can reach it. It agrees to Alice's terms on the agent's
    behalf by itself, within `UMA4A_STANDING_MAX_EXPIRES` (7 days): no person
    at S&V signs each agreement, and it doesn't know which user a call is
    for ([ARCHITECTURE.md](ARCHITECTURE.md#what-the-lab-doesnt-enforce)).
  - Meridian: its gateway (agentgateway), `uma-pep` (ext-auth: challenges,
    introspection, proof-of-possession checks, tool-to-resource scoping,
    single-use consumption) and `alice-vault` (a stock MCP server with no
    auth code). The gateway is reachable from the edge only; the vault from
    the gateway and Alice's portal only.
  - Alice: `alice/uma-as` (her authorization server and its Postgres, run by
    CloudNativePG), `alice/portal`, and her IdP (`alice-identity`, realm
    `alice`). Her grant surface is reachable through the edge only; her owner
    API only from her portal, or with her own token.
- **Every cross-party hop crosses the edge**, the way separate companies
  meet on the internet: adapter to Meridian, Meridian to Alice's AS, adapter
  to Alice's AS.
- **Her terms, by tier:**

| Tier | Resource | Her default | What the agent gets |
| --- | --- | --- | --- |
| 1 | holdings | approve on first contact, then standing | a grant covered by her terms |
| 2 | transactions | approve once per tier | the same, for this tier |
| 3 | trades | ask me, every time | a single-use grant for that one operation |

  An agent with no standing connection is held on first contact whatever the
  tier. Revoking the agent in her portal kills every token it holds; its next
  request starts over as a stranger.
- **Proof of possession.** An RPT is bound to the agent's key, and every
  request is signed (RFC 9421). A copied RPT is useless without the key.
- **What stays where.** Meridian enforces Alice's terms without reading them.
  S&V never sees Alice's credentials, and Alice never needs to know how S&V
  identifies its agent.

Checks (`make alice-verify`):

| Case | Expected |
| --- | --- |
| tier 1, first contact | held for Alice; approved; holdings returned |
| tier 2 | held at the new tier, approved, history returned; asked again, it goes through on her standing terms |
| tier 3 trade | held; Alice denies; nothing executed |
| a call to Meridian with no grant | 401 with a UMA challenge |
| Alice's owner API without her token | refused |
| another S&V workload using Bob's agent's adapter | refused (mesh) |
| straight to Alice's vault, skipping Meridian's gateway | refused (mesh) |
| straight to Alice's AS, skipping the edge | refused (mesh) |

`make reset` clears Alice's grants, ledger and terms, and gives the adapter a
new agent key, so the next run starts as a first contact.

## Patches these flows need

| Patch | Needed for | Why |
| --- | --- | --- |
| `tools/kagent` 0001 | all three | Substrate actors call the kagent controller back with the caller's credential (they have no ServiceAccount token) |
| `tools/kagent` 0002 | writes that wait for approval, UMA holds | a turn sent as the last one closes (after a human approval) no longer races the actor's suspend (OSS controller) |
| `tools/substrate-mesh` 0001, 0002 | all three | a worker pool runs as its agent's ServiceAccount, so the agent's calls carry that SPIFFE ID (which the waypoint, ai-gateway and the adapter check), and only serves its own namespace |
| `tools/substrate-mesh` 0003 | all three | actors work under the mesh's in-pod traffic capture |
| `tools/keycloak-idjag` | Cross App Access | ID-JAG issuing (keycloak/keycloak#49998) |

## Seeing them in the Observatory

- **Topology, Identity view:** the IdPs, SSO links, token-exchange hops and
  gateways that check tokens, with each agent's path to its tools.
- **Traffic, Carries a token:** each request's verified claims at the gateway
  that checked them: at `mcp-waypoint` and `ai-gateway`, Bob's access token
  (issuer, audience, groups, expiry). Exchanged tokens are minted
  after the log point and aren't shown.
