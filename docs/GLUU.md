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
GLUU_CLIENT_ID=<S&V's broker client at that Gluu>
GLUU_CLIENT_SECRET=<its secret>
RESOURCE_AS=gluu
RESOURCE_AS_ISSUER=https://<Ledgerline's Gluu>
```

| `ENTERPRISE_IDP` | `RESOURCE_AS` | ID-JAG issuer | Redeemed by |
| --- | --- | --- | --- |
| `keycloak` / `auth0,keycloak` | `keycloak` | S&V Keycloak | Ledgerline Keycloak |
| `keycloak` / `auth0,keycloak` | `gluu` | S&V Keycloak | Ledgerline's Gluu |
| `gluu,keycloak` | `keycloak` | Gluu (S&V Keycloak while Gluu is down) | Ledgerline Keycloak |
| `gluu,keycloak` | `gluu` | Gluu (S&V Keycloak while Gluu is down) | Ledgerline's Gluu |

`RESOURCE_AS_ISSUER` must be a different deployment from `GLUU_ISSUER`; the
install refuses one Gluu in both roles. Issuers are compared as strings:
use the value discovery states.

## S&V enterprise IdP: Gluu

Registration at S&V's Gluu:

| Item | Value |
| --- | --- |
| Client | confidential, `client_secret_basic` |
| Redirect URI | `https://idp.sterling.lab/realms/sterling-vance/broker/gluu/endpoint` |
| Post-logout redirect URI | `https://idp.sterling.lab/realms/sterling-vance/broker/gluu/endpoint/logout_response` |
| Grant types | `authorization_code` (PKCE S256), `refresh_token`, `urn:ietf:params:oauth:grant-type:token-exchange` |
| Scopes | `openid email profile offline_access`, `xaa-ledgerline`, `research:read` |
| Refresh tokens | issued with `offline_access`; not rotated, or reuse tolerated (S&V's egress runs two replicas) |
| User | `bob`, email `bob@sterling.lab`, `email_verified: true`; passkey enrolled |
| Default ACR | passkey (`fido2`) |
| ID-JAG for this client | audience the value of `RESOURCE_AS`'s issuer (`https://idp.ledgerline.lab/realms/ledgerline` with `RESOURCE_AS=keycloak`); claim `client_id` = `sterling-vance-kagent`; `typ` `oauth-id-jag+jwt`; lifetime ≤ 300 s; subject token types: ID token |

In the lab:

1. `.env` as above; `make layer-45 layer-47` (realm, continuity tier `gluu`,
   first).
2. Sign in once as Bob at `https://kagent.sterling.lab`: the login goes to
   Gluu. S&V's Keycloak links the account by verified email and keeps Bob's
   Gluu tokens.
3. `./demos/bob/install.sh`: Ledgerline trusts Gluu as an ID-JAG issuer and
   links Bob's Ledgerline account to his Gluu `sub`.
4. `make bob-verify`: the ID-JAG check expects `gluu`.

Ledgerline's Keycloak fetches Gluu's JWKS through `ledgerline-egress`.

## Ledgerline authorization server: Gluu

Registration at Ledgerline's Gluu:

| Item | Value |
| --- | --- |
| Trusted ID-JAG issuers | each IdP in `ENTERPRISE_IDP` that vouches: S&V's Gluu (its `jwks_uri`) and `https://idp.sterling.lab/realms/sterling-vance` (static JWKS, `make xaa-keys` → `sv-idp.jwks.json`; that issuer is not reachable from the internet) |
| Client | `sterling-vance-kagent`, `private_key_jwt` RS256, JWKS `sv-client.jwks.json` (`make xaa-keys`) |
| Grant type | `urn:ietf:params:oauth:grant-type:jwt-bearer`; assertion reuse refused; assertion lifetime ≤ 300 s |
| Subject mapping | ID-JAG `sub` per issuer to Ledgerline's Bob |
| Access token | JWT, `aud` `ledgerline-research`, scope `research:read`, claim `user_name` or `email`, lifetime ≤ 300 s |

In the lab: `.env` as above, `./demos/bob/install.sh`. Ledgerline's MCP
server and waypoint accept tokens from `RESOURCE_AS_ISSUER` only; the server
fetches its JWKS through `ledgerline-egress`.

## Logs

`make xaa-logs SINCE=2h` writes `.lab/xaa/<UTC time>/`; tokens are replaced
by `<jwt>`, claims kept. In `xaa-relay.log`:

| Message | Fields |
| --- | --- |
| `id-jag accepted` | `idp`, `params` (credentials redacted, a JWT's `jti` kept), `header` (`typ`, `alg`, `kid`), `claims` (`iss`, `sub`, `aud`, `client_id`, `scope`, `jti`, `exp`) |
| `id-jag rejected` | the same, and `reason` |
| `access token issued` | `params` (`assertion` by its `jti`), `client` (`private_key_jwt`), `claims` of Ledgerline's token |
| `token request refused` | `error` from the token endpoint |

## Roadmap

Not built. Each item, with what it needs.

| Item | Needs |
| --- | --- |
| CIMD client ID for S&V's client at the AS | HTTPS-hosted client metadata document whose URL is the `client_id`, `jwks` from `make xaa-keys`; AS that dereferences CIMD at the token endpoint. agentgateway `clientId` takes the URL as is |
| Resource indicators (RFC 8707) | `crossAppAccess.resources: [https://mcp.ledgerline.lab/mcp]`; an AS that binds `aud` to `resource` (Gluu: confirm; Ledgerline Keycloak: not supported) |
| Sender-constrained tokens (DPoP, RFC 9449) | agentgateway DPoP proof on the RAS leg and on the MCP call; AS issuing `cnf.jkt`; Ledgerline's waypoint and server checking the proof |
| MCP enterprise-managed authorization extension | agentgateway or kagent MCP client declaring it on `initialize` |
| OAuth MCP flow (401 + `WWW-Authenticate` to OPRM) | MCP server on a public HTTPS host; agentgateway serving Protected Resource Metadata and the challenge for `/mcp` |
| ID-JAG checks and redacted token-request logs inside agentgateway | crossAppAccess validating `typ`/`iss`/`aud`/signature and logging both legs; then xaa-relay goes |
| Broker client at Gluu with `private_key_jwt` | IdentityContinuity `oidc.clientAuthMethod` and Keycloak IdP "JWT signed with private key" in the controller |
| Shared refresh-token state for idtoken-exchange | a store shared by replicas (or one replica) for upstreams that rotate refresh tokens with reuse detection |
| Token status list / revocation at the RS | Ledgerline's server checking `status` (Gluu `status_list_endpoint`) per call |
| AuthZEN policy decision at the egress | ext-auth adapter calling the PDP's `/access/v1/evaluation` (subject Bob, action tool call, resource tool) before the XAA exchange |
| TRACE evidence for each agent action | emitter from xaa-relay and gateway logs: EAT claims (agent SPIFFE ID, model, policy hash), references to the ID-JAG `jti` and tool call, SCITT registration |
| Hardware-anchored agent identity | SPIRE with TPM node attestation (swtpm in kind, vTPM on a hosted cluster) issuing the agents' SPIFFE IDs |
| Gluu in the lab | Janssen Helm chart per party with CloudNativePG, clients, scopes, users and trusted issuers configured by script |
| Gluu as S&V's only IdP | S&V's services (edge SSO, waypoint delegation, registry, Observatory) on Gluu clients; Gluu token exchange for the waypoint audience and for access token to ID token; continuity without a broker |
