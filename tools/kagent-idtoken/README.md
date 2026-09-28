# kagent: forward the user's OIDC ID token to agents (upstream PR candidate)

**Why.** In Cross App Access (XAA, ID-JAG), the enterprise IdP issues an ID-JAG
only for an **ID token issued to the requesting app**, the app the user
signed into. In this lab that app is kagent. kagent 0.10.2 in trusted-proxy mode
forwards only `Authorization` (the access token) and `X-User-Id` to agents, so
no agent can take part in XAA without something re-minting the ID token.
Re-minting isn't acceptable in a reference architecture.

**What the patch does** (`patches/0001-…patch`, against v0.10.2):

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

**Build:** `./build.sh` → `localhost:5001/kagent-dev/kagent/controller:0.10.2-idtoken.1`.
It reproduces `make build-controller`: the upstream Dockerfile, version ldflags,
and the **released** 0.10.2 runtime-image digests (`released-digests.env`,
checked against the released controller binary). Only the controller image
changes; agents keep running the stock 0.10.2 runtimes.

**Upstream:** open against `kagent-dev/kagent` main. Once it's released,
remove `controller.image` from `platform/60-kagent/values.yaml` and move the
`AUTH_ID_TOKEN_*` env to `controller.auth.idToken`.
