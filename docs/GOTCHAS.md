# Gotchas (each one cost real time in this lab)

**Mesh**
- **agentgateway native HBONE bypasses destination waypoints.** With
  `istio.enabled` set it dials pods directly. It's right when agentgateway *is*
  the waypoint (sv-mcp). Where traffic must pass an Istio waypoint (Meridian),
  run the gateway ztunnel-captured instead (`istio.enabled: false`).
- **Pods that terminate HBONE themselves need `networking.istio.io/tunnel: http`.**
  Without it callers' ztunnels send plaintext and CEL's `source.identity` is empty.
- **`agentgateway.dev/internal-ports` drops that port from the Service.** Only use
  it for the waypoint pattern.
- **Gateways send to pod addresses.** A waypoint that must see edge traffic needs
  `istio.io/waypoint-for: all` with the *pods* labelled `use-waypoint`.
- **A namespace-wide `use-waypoint` also captures databases.** Their callers then
  arrive as the waypoint's identity and L4 policies refuse them. Opt in per workload.
- **Re-applying `namespaces.yaml` resets `use-waypoint` labels.** Keep the file the
  source of truth.
- **One `source` block ANDs its fields**; separate `from` entries OR. Istio
  principals only allow `*` at the start or end.
- **Istio `AuthorizationPolicy.selector` has no `matchExpressions`.**

**Identity**
- **`*.localhost` doesn't work as an issuer.** hickory (agentgateway) and newer Go
  resolvers answer it with loopback, never asking DNS. The lab uses `*.lab`
  with a CoreDNS rewrite.
- **Keycloak's dev store rotates signing keys and user ids on every restart.** Pin
  both in the realm file (the `rsa` key provider, and user `id`).
- **Defining any `clientScopes` in a realm import drops the built-in scopes.** Carry
  them explicitly.
- **Keycloak standard token exchange only accepts an *access* token as subject.**
  PR #49998 only accepts an *ID* token. Hence kgateway's OAuth2 filter forwards
  the access token, and the kagent patch carries the ID token separately.
- **agentgateway strips a validated JWT before backend auth.** Feed the exchange
  from CEL (`jwt.rawToken.unredacted()`) rather than `preserveToken`.
- **agentgateway `crossAppAccess` sends `scopes` to the resource AS too**, unless
  `accessTokenScopes: []`.
- **`oauthTokenExchange` forwards only bearer tokens.** An ID-token result (`N_A`)
  is rejected by design, so don't chain through it.
- **Pointing the IdP redirector at a provider needs `manage-realm`.** Authenticator
  configs are realm-level; `manage-identity-providers` alone gets a 403.
- **Keycloak allows one client authenticator per client.** The edge and a gateway
  can't hold different credentials for the same client.

**kagent**
- **trusted-proxy mode decodes the JWT without verifying it.** Fence the controller
  with mesh policy.
- **The controller discovers MCP tools with no user token.** Give it its own lane
  (by SPIFFE identity) rather than opening the user lane.
- **The controller forwards only `Authorization` and `X-User-Id` to agents** (hence
  the ID-token patch).

**Agent Substrate**
- **In an ambient namespace, stock 0.0.9 actors never get ready.** ztunnel captures
  ateom's readyz to the actor veth and inbound bypasses the mesh (tools/substrate-mesh).
- **Worker pods need two annotations:** `istio.io/reroute-virtual-interfaces: ateom0`
  (actor egress through ztunnel, else it leaves with no identity) and
  `ambient.istio.io/dns-capture: "false"` (else the actor's DNS replies are dropped).
- **Labelling ate-system ambient isn't enough.** istio-cni's `excludeNamespaces` wins
  (it only logs a warning), and a changed exclude list needs an istio-cni-node restart.
- **Worker selectors match pool labels across namespaces**, and every worker used to
  run as `default`. Name pools uniquely; identity-bearing pools are namespace-private.
- **ate-api authenticates any ServiceAccount token for its audience and authorizes
  nothing.** Fence it (and the router, which reaches any actor by name) by identity.
- **ate-api resolves an actor's secret env itself**, and the kagent chart only lets it
  read Secrets in kagent's namespace. Each agent namespace grants its own (by name).
- **Stock kagent 0.10.2 SandboxAgents 401 in trusted-proxy mode** (no ServiceAccount
  token to call back with), and a turn sent as the previous one closes (a HITL
  approval) can hang on its suspend. tools/kagent 0002 and 0003.
- **SandboxAgents need a `contextId` on every A2A message**, at
  `/api/a2a-sandboxes/<ns>/<name>`, and can't share a name with an Agent.
- **A failed resume keeps its worker claimed** until that same session resumes or the
  worker pod goes; the pool reports "no free workers". atelet's image cache is in
  memory, so a restarted atelet's first pull can outlast the controller's 30s resume.

**Platform**
- **Substrate 0.0.9 is the pairing kagent 0.10.x vendors.** 0.0.13+ needs an
  out-of-band CA-pool bootstrap, and in-place 0.0.x upgrades corrupt valkey state.
- **Istio 1.31 charts aren't on the GCS Helm repo.** They come from the verified
  release tarball.
- **`port_forward` sets an EXIT trap.** A script's own cleanup trap must be set after it.
- **macOS ships bash 3.2**: no `mapfile`, and empty arrays under `set -u` need
  `${a[@]+"${a[@]}"}`. zsh doesn't word-split `set -- $var`.
