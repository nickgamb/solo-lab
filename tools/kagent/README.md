# kagent 0.10.2 patches (upstream PR candidates)

Three patches against v0.10.2, each its own upstream PR. `./build.sh` builds
the controller and the Go ADK from them.

## 0001: forward the user's OIDC ID token to agents

**Why.** In Cross App Access (XAA, ID-JAG), the enterprise IdP issues an ID-JAG
only for an **ID token issued to the requesting app**, the app the user
signed into. In this lab that app is kagent. kagent 0.10.2 in trusted-proxy mode
forwards only `Authorization` (the access token) and `X-User-Id` to agents, so
no agent can take part in XAA without something re-minting the ID token.
Re-minting isn't acceptable in a reference architecture.

**What it does:**

- New controller settings: `controller.auth.idToken.{header,cookie,audience}`
  (`AUTH_ID_TOKEN_HEADER` / `_COOKIE` / `_AUDIENCE`). They say where the ID token
  arrives; the cookie form covers the `IdToken` cookie an Envoy/kgateway OAuth2
  filter sets.
- On A2A calls to agents the controller adds `X-Id-Token`, **only when the token
  is bound to the authenticated user**:
  - same `sub` as the access token
  - issued to `audience` (`aud` or `azp`)
  - not expired

  Anything else is dropped. It is never used to authenticate, so
  `Authorization` semantics are unchanged.
- Agents already forward selected headers to MCP servers
  (`tools[].mcpServer.allowedHeaders`), so the ID token reaches **only the tools
  that need it**. That's least privilege, and no ADK change is needed.
- Tests: `TestProxyAuthenticator_ForwardsBoundIDToken`, eight cases (header,
  cookie, precedence, other user's token, wrong client, expired, malformed,
  not configured). The existing auth-mode tests move to keyed literals.

## 0002: Substrate actors call kagent back with the caller's credential

**Why.** Agents call the controller back (sessions, task store) with their
projected ServiceAccount token. Agent Substrate actors have no projected
volumes, so a SandboxAgent sends no `Authorization` at all, and in
trusted-proxy mode every callback is a 401: no SandboxAgent can hold a
conversation.

**What it does:** when the agent has no token of its own, the Go ADK
authenticates the callback with the bearer token of the inbound A2A request
it is serving (the credential kagent forwarded for this user). Nothing new is
minted or stored, so nothing lands in Substrate snapshots. Agents with a
ServiceAccount token are unchanged. Test: `TestAddHeaders_Authorization`.

## 0003: a turn that follows a response closely no longer hangs

**Why.** When a SandboxAgent response closes, the controller schedules a
suspend of the session actor. A message sent right away, a HITL approval for
one, races it: arriving mid-suspend it waits two minutes for an actor nobody
resumes; arriving just before, the actor is checkpointed under it.

**What it does:** the session transport counts in-flight turns per session.
A suspend holds the session's lock while it checks and suspends; a new turn
registers, then waits out a suspend in progress. For suspends another
controller replica started, `EnsureSessionActor` waits out `SUSPENDING`, then
resumes. Test: `TestSessionTurns_SuspendNeverLandsOnANewerTurn`.

## Build

`./build.sh` → `localhost:5001/kagent-dev/kagent/{controller,golang-adk}:0.10.2-lab.3`.
It reproduces `make build-controller`: the upstream Dockerfile, version
ldflags, and the runtime-image digests baked into the controller. golang-adk
is built from the patched source; every other runtime image keeps the
**released** 0.10.2 digest (`released-digests.env`, checked against the
released controller binary), and golang-adk-full is mirrored here by digest
because one registry (`controller.goAgentImage.registry`) serves both Go
variants. Each build re-snapshots every SandboxAgent (a new golang-adk digest
is a new ActorTemplate).

**Upstream:** open each against `kagent-dev/kagent` main. Once they ship,
remove `controller.image` and `controller.goAgentImage` from
`platform/60-kagent/values.yaml` and move the `AUTH_ID_TOKEN_*` env to
`controller.auth.idToken`.
