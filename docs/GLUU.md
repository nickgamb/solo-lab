# Gluu (experimental)

Story 1's Cross App Access with Gluu Flex / Janssen in both companies'
slots: S&V's enterprise IdP (signs Bob in, issues his ID-JAG) and
Ledgerline's authorization server (redeems it). Same code path as the
default; two settings choose who plays each role
([IDENTITY-FLOWS.md](IDENTITY-FLOWS.md#2-cross-app-access-id-jag-to-a-saas)).

- Demo card: [cards/gluu.html](cards/gluu.html)
- Checks: `make bob-verify` (Bob signs in through S&V's own Keycloak, which
  vouches); [Run it](#run-it) for Gluu vouching
- Logs and keys: `make xaa-logs`, `make xaa-keys`

## Run it

With both Gluu servers set up as below, from a fresh clone:

```bash
cp .env.example .env     # then the Gluu settings below
make xaa-keys            # S&V's public keys, before the lab exists
make machine-setup       # once per machine
make up
```

Register the keys `make xaa-keys` wrote to `.lab/xaa/keys/` before `make up`:

| File | Register at |
| --- | --- |
| `sv-upstream-client.jwks.json` | S&V's Gluu: S&V's client (`GLUU_CLIENT_ID`) |
| `sv-ras-client.jwks.json` | Ledgerline's Gluu: S&V's client (`LEDGERLINE_GLUU_CLIENT_ID`) |
| `sv-idp.jwks.json`, `sv-workforce.jwks.json` | Ledgerline's Gluu, as trusted ID-JAG issuers `https://idp.sterling.lab/realms/sterling-vance` and `https://login.sterling.lab/realms/workforce`: then sessions S&V's broker or Keycloak vouches for are accepted too |

Then sign in as Bob at `https://kagent.sterling.lab`: the login goes to
Gluu (a passkey; the first sign-in enrolls one). Ask his assistant which
Ledgerline account he is using: Gluu vouches (an ID-JAG) and Ledgerline's
Gluu redeems it. `make xaa-logs` writes the trail.

## Settings

`.env` (each setting: [IDPS.md](IDPS.md#gluu-gluu)):

```
ENTERPRISE_IDP=gluu,auth0,keycloak,contingency
GLUU_ISSUER=https://<S&V's Gluu>
GLUU_CLIENT_ID=<S&V's client at that Gluu>
GLUU_DIRECTORY_CLIENT_ID=<the directory sync's client there>
GLUU_DIRECTORY_CLIENT_SECRET=<its secret>
RESOURCE_AS=gluu
LEDGERLINE_GLUU_ISSUER=https://<Ledgerline's Gluu>
LEDGERLINE_GLUU_CLIENT_ID=<GLUU_CLIENT_ID>
```

Gluu first makes it S&V's primary: the source of truth for who exists and
their groups. The directory sync reads its users over SCIM and gives the
broker an account for each whose email is verified (`emailVerified` in its
SCIM user extension) under `sterling.lab`; Bob signs in to that account.
Janssen puts the requesting client's own ID in an ID-JAG, so S&V registers at
Ledgerline's Gluu under its Gluu client ID, and S&V's own Keycloak and broker
put that ID in their ID-JAGs too. Ledgerline's Gluu must be a different
deployment from S&V's; the install refuses one Gluu in both roles.

## S&V's Gluu

- S&V's client: [Registering S&V at an IdP](IDPS.md#registering-sv-at-an-idp),
  with `private_key_jwt` and token exchange (access token to ID token, ID
  token to ID-JAG for Ledgerline's Gluu).
- A `groups` claim in the ID token for that client, with the user's Gluu
  groups by name.
- User `bob`, email `bob@sterling.lab` (verified), in group `advisors`.
- A SCIM client for the directory sync ([IDPS.md](IDPS.md#gluu-gluu)).

## Ledgerline's Gluu

[Ledgerline's authorization server](IDPS.md#ledgerlines-authorization-server):
S&V's client with `sv-ras-client.jwks.json`, the jwt-bearer grant, S&V's Gluu
as a trusted ID-JAG issuer, scope `research:read`, access tokens for
`ledgerline-research`.

After failover to the next IdP a Gluu session is refused; Bob signs in again
there and that IdP (or, if it doesn't issue ID-JAGs, S&V's broker) vouches.
Ledgerline's Gluu accepts that only if it trusts that issuer's keys.

## Logs

`make xaa-logs SINCE=2h` writes `.lab/xaa/<UTC time>/`; tokens are replaced
by `<jwt>`, claims kept. agentgateway makes both token requests through its
own routes, so ai-gateway's access log (`ai-gateway.log`) has them:

| Route | Fields |
| --- | --- |
| `xaa-ledgerline` (rule: the IdP that vouches) | the caller's SPIFFE ID, the verified S&V token's claims, MCP method and tool, status |
| `xaa-idp-<idp>` | the verified ID token's claims (`iss`, `sub`, `aud`, `acr`, `amr`), the IdP's endpoint, status |
| `xaa-as-ledgerline` | the verified ID-JAG's claims (`iss`, `sub`, `aud`, `client_id`, `scope`, `jti`, `iat`, `exp`), status; 401 or 403 when it fails a check |

## Roadmap

Not built. Each item, with what it needs.

| Item | Needs |
| --- | --- |
| Keys for `kagent` too | kgateway OAuth2 with `private_key_jwt`; then `kagent` (edge and egress) authenticates with one key per component, and no secret is left |
| Upstream logout reaching S&V | OIDC back-channel logout from the upstream to S&V's broker, ending the Keycloak session (the broker's session otherwise outlives it) |
| CIMD client ID for S&V's client at the AS | an HTTPS-hosted client metadata document whose URL is the `client_id`, `jwks` from `make xaa-keys`; an AS that dereferences CIMD at the token endpoint |
| Resource indicators (RFC 8707) | `crossAppAccess.resources: [https://mcp.ledgerline.lab/mcp]`; an AS that binds `aud` to `resource` |
| Sender-constrained tokens (DPoP, RFC 9449) | agentgateway DPoP proofs on the RAS leg and the MCP call; an AS issuing `cnf.jkt`; Ledgerline's MCP gateway and server checking the proof |
| MCP enterprise-managed authorization extension | agentgateway or kagent MCP client declaring it on `initialize` |
| OAuth MCP flow (401 + `WWW-Authenticate` to Protected Resource Metadata) | the MCP server on a public HTTPS host; agentgateway serving the metadata and the challenge for `/mcp` |
| Path-level policy on S&V's broker | a waypoint for `sv-identity`, so each caller reaches only its endpoints (the egress: token, certs, broker token; never admin) |
| Narrower rights for the continuity controller and its sync | the controller holds `manage-identity-providers` and `manage-realm` (the login redirector lives in an authentication flow); the sync `manage-users` at the broker and at each Keycloak directory: Keycloak fine-grained admin permissions scoped to the IdPs, that one flow and attribute writes, or each credential treated as tier 0 (rotation, monitoring) |
| Token status list / revocation at the RS | Ledgerline's server checking `status` (Gluu `status_list_endpoint`) per call |
| AuthZEN policy decision at the egress | an ext_proc calling the PDP's `/access/v1/evaluation` (subject Bob, action tool call, resource tool) before the XAA exchange |
| TRACE evidence for each agent action | an emitter from the gateway's access log: EAT claims (agent SPIFFE ID, model, policy hash), references to the ID-JAG `jti` and tool call, SCITT registration |
| Hardware-anchored agent identity | SPIRE with TPM node attestation (swtpm in kind, vTPM on a hosted cluster) issuing the agents' SPIFFE IDs |
| S&V's issuers trusted from outside the lab | S&V's broker and Keycloak issuers on public hosts (the lab hosted, with public DNS and certificates), or their JWKS published where an AS outside the lab can fetch them |
| Gluu in the lab | the Janssen Helm chart per party with CloudNativePG, clients, scopes, users and trusted issuers configured by script |
| Gluu as S&V's only IdP | S&V's services (edge SSO, waypoint delegation, registry, Observatory) on Gluu clients; Gluu token exchange for the waypoint audience and for access token to ID token; continuity without a broker |
