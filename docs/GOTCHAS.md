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
- **Keycloak allows one client authenticator per client.** The edge and a gateway
  can't hold different credentials for the same client.

**kagent**
- **trusted-proxy mode decodes the JWT without verifying it.** Fence the controller
  with mesh policy.
- **The controller discovers MCP tools with no user token.** Give it its own lane
  (by SPIFFE identity) rather than opening the user lane.
- **The controller forwards only `Authorization` and `X-User-Id` to agents** (hence
  the ID-token patch).

**Platform**
- **Substrate 0.0.9 is the pairing kagent 0.10.x vendors.** 0.0.13+ needs an
  out-of-band CA-pool bootstrap, and in-place 0.0.x upgrades corrupt valkey state.
- **Istio 1.31 charts aren't on the GCS Helm repo.** They come from the verified
  release tarball.
- **macOS ships bash 3.2**: no `mapfile`, and empty arrays under `set -u` need
  `${a[@]+"${a[@]}"}`. zsh doesn't word-split `set -- $var`.
