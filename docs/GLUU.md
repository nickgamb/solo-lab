# Gluu

Story 1's Cross App Access with Gluu Flex / Janssen in both companies'
slots: S&V's enterprise IdP (signs Bob in, issues his ID-JAG) and
Ledgerline's authorization server (redeems it). Same code path as the
default; two settings choose who plays each role
([IDENTITY-FLOWS.md](IDENTITY-FLOWS.md#2-cross-app-access-id-jag-to-a-saas)).

- Demo card: [cards/gluu.html](cards/gluu.html)
- Checks: `make bob-verify`, `make continuity-verify`
- Logs and keys: `make xaa-logs`, `make xaa-keys`

## Settings

`.env`:

```
ENTERPRISE_IDP=gluu,keycloak
GLUU_ISSUER=https://<S&V's Gluu>
GLUU_CLIENT_ID=<S&V's client at that Gluu>
RESOURCE_AS=gluu
RESOURCE_AS_ISSUER=https://<Ledgerline's Gluu>
```

| `ENTERPRISE_IDP` | `RESOURCE_AS` | Vouches for a session through Gluu | Redeemed by |
| --- | --- | --- | --- |
| `keycloak` / `auth0,keycloak` | `keycloak` | (no Gluu) S&V Keycloak | Ledgerline Keycloak |
| `keycloak` / `auth0,keycloak` | `gluu` | (no Gluu) S&V Keycloak | Ledgerline's Gluu |
| `gluu,keycloak` | `keycloak` | Gluu | Ledgerline Keycloak |
| `gluu,keycloak` | `gluu` | Gluu | Ledgerline's Gluu |

After failover to S&V's own accounts a Gluu session is refused; Bob signs in
again and S&V's Keycloak vouches. `RESOURCE_AS_ISSUER` must be a different
deployment from `GLUU_ISSUER`; the install refuses one Gluu in both roles.
Issuers are compared as strings: use the value discovery states.

S&V authenticates to both Gluus with keys (`private_key_jwt`), never a shared
secret; client assertions name the authorization server by its issuer
(`aud`). `make xaa-keys` writes the public keys each side registers
(`.lab/xaa/keys/`); every key is stable across rebuilds.

## S&V enterprise IdP: Gluu

Registration at S&V's Gluu:

| Item | Value |
| --- | --- |
| S&V's client | confidential, `private_key_jwt`; JWKS `sv-upstream-client.jwks.json` (the broker's key, PS256, and the egress's key, RS256); its client ID into `GLUU_CLIENT_ID` |
| Redirect URI | `https://idp.sterling.lab/realms/sterling-vance/broker/gluu/endpoint` |
| Grant types | `authorization_code` (PKCE S256), `refresh_token`, `urn:ietf:params:oauth:grant-type:token-exchange` |
| Scopes | `openid email profile offline_access`, `xaa-ledgerline`, `research:read` |
| Refresh tokens | issued with `offline_access`, not rotated (a confidential client authenticating with a key; RFC 9700 4.14.2) |
| ID-JAG for that client | subject: ID token; `aud` Ledgerline's AS issuer (`https://idp.ledgerline.lab/realms/ledgerline` with `RESOURCE_AS=keycloak`); `client_id` `sterling-vance-kagent`; `typ` `oauth-id-jag+jwt`; `iat`; at most 300 s |
| Ledgerline's SSO client | client ID `ledgerline`, `private_key_jwt`, JWKS `ledgerline-sso-client.jwks.json`; redirect `https://idp.ledgerline.lab/realms/ledgerline/broker/sterling-vance-gluu/endpoint`; PKCE S256; scopes `openid email profile` |
| User | `bob`, email `bob@sterling.lab`, `email_verified: true`; passkey (default ACR `fido2`) |

In the lab:

1. `.env` as above; `make layer-45 layer-47 && ./demos/bob/install.sh`.
2. Sign in as Bob at `https://kagent.sterling.lab`: the login goes to Gluu.
   S&V's Keycloak links Bob's S&V account by verified email and keeps his
   Gluu tokens. Platform admins (role `local-only`) are never linked to an
   upstream.
3. Sign in once to Ledgerline as Bob through Gluu
   (`https://idp.ledgerline.lab/realms/ledgerline/account`): Ledgerline links
   his account to his Gluu identity, for S&V's domain only.
4. In kagent, ask which Ledgerline account Bob is using: the ID-JAG comes
   from Gluu (`make xaa-logs`, `xaa-relay.log`, `idp: gluu`).

## Ledgerline authorization server: Gluu

Registration at Ledgerline's Gluu:

| Item | Value |
| --- | --- |
| Trusted ID-JAG issuers | each of S&V's IdPs that vouch: S&V's Gluu (its `jwks_uri`) and `https://idp.sterling.lab/realms/sterling-vance` (JWKS `sv-idp.jwks.json`; that issuer is not reachable from the internet) |
| S&V's client | `sterling-vance-kagent`, `private_key_jwt` RS256, JWKS `sv-ras-client.jwks.json` |
| Grant type | `urn:ietf:params:oauth:grant-type:jwt-bearer`; assertion reuse refused; assertion lifetime at most 300 s |
| Subject mapping | the ID-JAG `sub` per issuer to Ledgerline's Bob |
| Access token | JWT; `aud` `ledgerline-research`; scope `research:read` when requested; `client_id` `sterling-vance-kagent`; `user_name` or `email`; at most 300 s |

In the lab: `.env` as above, `./demos/bob/install.sh`. Ledgerline's MCP
server and waypoint accept tokens from `RESOURCE_AS_ISSUER` only, and the
server requires scope `research:read` from client `sterling-vance-kagent`.
It fetches the AS's JWKS through `ledgerline-egress`.

## Logs

`make xaa-logs SINCE=2h` writes `.lab/xaa/<UTC time>/`; tokens are replaced
by `<jwt>`, claims kept. In `xaa-relay.log`:

| Message | Fields |
| --- | --- |
| `id-jag accepted` | `idp`, `params` (credentials redacted, a JWT's `jti` kept), `client` (method), `header` (`typ`, `alg`, `kid`), `claims` (`iss`, `sub`, `aud`, `client_id`, `scope`, `jti`, `iat`, `exp`) |
| `id-jag rejected` | the same, and `reason` |
| `access token issued` | `params` (`assertion` by its `jti`), `client` (`private_key_jwt`), `claims` of Ledgerline's token |
| `token request refused` | `reason` (not this connection's request) or `error` from the token endpoint |

## Roadmap

Not built. Each item, with what it needs.

| Item | Needs |
| --- | --- |
| Keys for `kagent` too | kgateway OAuth2 with `private_key_jwt`; then `kagent` (edge and egress) authenticates with one key per component, and no secret is left |
| Upstream logout reaching S&V | OIDC back-channel logout from the upstream to S&V's broker, ending the Keycloak session (offline tokens otherwise outlive it) |
| CIMD client ID for S&V's client at the AS | an HTTPS-hosted client metadata document whose URL is the `client_id`, `jwks` from `make xaa-keys`; an AS that dereferences CIMD at the token endpoint |
| Resource indicators (RFC 8707) | `crossAppAccess.resources: [https://mcp.ledgerline.lab/mcp]`; an AS that binds `aud` to `resource` |
| Sender-constrained tokens (DPoP, RFC 9449) | agentgateway DPoP proofs on the RAS leg and the MCP call; an AS issuing `cnf.jkt`; Ledgerline's waypoint and server checking the proof |
| MCP enterprise-managed authorization extension | agentgateway or kagent MCP client declaring it on `initialize` |
| OAuth MCP flow (401 + `WWW-Authenticate` to Protected Resource Metadata) | the MCP server on a public HTTPS host; agentgateway serving the metadata and the challenge for `/mcp` |
| ID-JAG checks and redacted token-request logs inside agentgateway | crossAppAccess validating the ID-JAG and logging both legs; then xaa-relay goes |
| Path-level policy on S&V's Keycloak | a waypoint for `sv-identity`, so each caller reaches only its endpoints (the egress: token, certs, broker token; never admin) |
| Narrower rights for the continuity controller | it holds `manage-identity-providers` and `manage-realm` (the login redirector lives in an authentication flow): Keycloak permissions scoped to the IdPs and that one flow, or its credential treated as tier 0 (rotation, monitoring) |
| Token status list / revocation at the RS | Ledgerline's server checking `status` (Gluu `status_list_endpoint`) per call |
| AuthZEN policy decision at the egress | an ext_proc calling the PDP's `/access/v1/evaluation` (subject Bob, action tool call, resource tool) before the XAA exchange |
| TRACE evidence for each agent action | an emitter from xaa-relay and gateway logs: EAT claims (agent SPIFFE ID, model, policy hash), references to the ID-JAG `jti` and tool call, SCITT registration |
| Hardware-anchored agent identity | SPIRE with TPM node attestation (swtpm in kind, vTPM on a hosted cluster) issuing the agents' SPIFFE IDs |
| Gluu in the lab | the Janssen Helm chart per party with CloudNativePG, clients, scopes, users and trusted issuers configured by script |
| Gluu as S&V's only IdP | S&V's services (edge SSO, waypoint delegation, registry, Observatory) on Gluu clients; Gluu token exchange for the waypoint audience and for access token to ID token; continuity without a broker |
