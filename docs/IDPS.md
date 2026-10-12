# IdPs and authorization servers

Two sets of identity providers, both chosen by name in `.env` and configured
the same way:

- **S&V's IdPs** (`ENTERPRISE_IDP`, in failover order; the first is the
  primary) sign S&V's people in. S&V's broker routes every sign-in to the
  active one and maps it into one shape
  ([IDENTITY-CONTINUITY.md](IDENTITY-CONTINUITY.md)). The broker vouches
  for S&V's people to other companies (the ID-JAG for Cross App Access),
  or, with `XAA_VOUCHER`, an IdP that issues ID-JAGs itself
  ([below](#an-idp-that-vouches)).

- **Ledgerline's authorization server** (`RESOURCE_AS`) redeems S&V's
  ID-JAGs for Ledgerline access tokens
  ([IDENTITY-FLOWS.md](IDENTITY-FLOWS.md#2-cross-app-access-id-jag-to-a-saas)).

Any combination works. A fresh clone runs `keycloak,contingency` (S&V's own
two, in the lab) and Ledgerline's own Keycloak, with nothing outside the lab.
An IdP without an issuer is left out of the chain.

| `ENTERPRISE_IDP` | `RESOURCE_AS` | `XAA_VOUCHER` | Bob signs in at | Vouches | Redeems |
| --- | --- | --- | --- | --- | --- |
| `keycloak,contingency` (a fresh clone) | `keycloak` | `broker` | S&V's Keycloak | S&V's broker | Ledgerline's Keycloak |
| `auth0,keycloak,contingency` | `keycloak` | `broker` | Auth0 | S&V's broker | Ledgerline's Keycloak |
| `gluu,auth0,keycloak,contingency` | `gluu` | `broker` | Gluu (passkey) | S&V's broker | Ledgerline's Gluu |
| `gluu,auth0,keycloak,contingency` | `gluu` | `gluu` | Gluu (passkey) | S&V's Gluu | Ledgerline's Gluu |
| `okta,keycloak` | `keycloak` | `broker` | Okta | S&V's broker | Ledgerline's Keycloak |
| `okta,keycloak` (experimental) | `auth0` | `okta` | Okta | Okta | Ledgerline's Auth0 |
| `okta,gluu,keycloak,contingency` (experimental) | `descope` | `okta` | Okta | Okta | Ledgerline's Descope |
| `ping,keycloak,contingency` | `keycloak` | `broker` | Ping | S&V's broker | Ledgerline's Keycloak |

When S&V's broker vouches, after a failover Bob signs in at the next IdP,
and the broker vouches for the same broker account. When an IdP vouches,
only its sign-ins reach Ledgerline.

## Settings

S&V's IdPs, by name (`NAME` is the name in upper case). Set the first three
in `.env`. `config/lab.env` has the rest for the IdPs below, and `.env`
overrides any of them. After a change, run `make layer-47`, then
`./demos/bob/install.sh` for Cross App Access.

| Setting | |
| --- | --- |
| `NAME_ISSUER` | its issuer, exactly as its discovery states it |
| `NAME_CLIENT_ID` | S&V's client there |
| `NAME_CLIENT_SECRET` | unset, S&V authenticates with its keys (`private_key_jwt`); set, `client_secret_post` |
| `NAME_DISPLAY_NAME` | its name on sign-in pages and in the Observatory |
| `NAME_GROUPS_CLAIM` | the ID token claim with the user's groups (or roles); default `groups` |
| `NAME_ASSURANCE` | the NIST 800-63B levels its sign-ins prove, by the `acr` or `amr` it asserts (JSON, [assurance](IDENTITY-CONTINUITY.md#assurance)); unset, AAL1 |
| `NAME_DIRECTORY_TYPE` | how the directory sync reads its users: `scim`, `auth0` or `keycloak`; unset, none |
| `NAME_DIRECTORY_URL` | the directory's API |
| `NAME_DIRECTORY_CLIENT_ID`, `NAME_DIRECTORY_CLIENT_SECRET` | a `client_credentials` client allowed to read users (and, for a failover, update them). The secret `lab:<NAME>_SECRET` is one the lab generates in `.lab/secrets.env`, under any name ending in `_SECRET` |
| `NAME_DIRECTORY_SCOPES`, `NAME_DIRECTORY_AUDIENCE` | what that client asks for |

Ledgerline's authorization server, by its name in `RESOURCE_AS`:

| Setting | |
| --- | --- |
| `LEDGERLINE_NAME_ISSUER` | its issuer, exactly as its discovery states it |
| `LEDGERLINE_NAME_CLIENT_ID` | S&V's client there; default `sterling-vance-kagent` |

The broker's ID-JAGs carry `LEDGERLINE_NAME_CLIENT_ID` as their `client_id`.
Ledgerline's AS is never S&V's broker or one of S&V's IdPs. One under `.lab`
must be Ledgerline's own Keycloak, which the lab runs. One outside the lab
must publish a token endpoint that is a plain `https` URL on its issuer's
host.

## Registering S&V at an IdP

The same for every IdP:

| Item | Value |
| --- | --- |
| Application | confidential OIDC web application |
| Redirect URI | `https://idp.sterling.lab/realms/sterling-vance/broker/<name>/endpoint` (also in `status.tiers[].redirectURI`) |
| Logout redirect | `https://idp.sterling.lab/realms/sterling-vance/broker/<name>/endpoint/logout_response` |
| Client authentication | `private_key_jwt` with the JWKS `sv-upstream-client.jwks.json` from `make xaa-keys` (the broker's key, PS256; `make xaa-keys` also runs before `make up`), or a client secret (`client_secret_post`) |
| Grant types | `authorization_code` with PKCE S256, `refresh_token` |
| Scopes | `openid email profile` |
| Users | `email` and `email_verified: true`, in the ID token or userinfo; the broker links a sign-in to the S&V account with that verified email |
| Groups | the user's groups by name in `NAME_GROUPS_CLAIM`, named as in `SV_GROUPS` (`advisors`, `platform-engineers`, `compliance`) |

The `.lab` hosts resolve only on your machine. An IdP only redirects the
browser to them, and every call S&V makes to it goes out from the lab.

An upstream needs no token exchange or ID-JAG setup while S&V's broker
vouches for everyone, whichever IdP signed them in.

### An IdP that vouches

`XAA_VOUCHER=<name>` has that IdP issue the ID-JAG instead of the broker
([IDENTITY-FLOWS.md](IDENTITY-FLOWS.md#2-cross-app-access-id-jag-to-a-saas)).
Only its sign-ins reach Ledgerline. It needs:

- S&V's client for ID-JAGs there: its sign-in client (`NAME_CLIENT_ID`), or
  one of its own (`XAA_CLIENT_ID`). It authenticates with
  `private_key_jwt`, the broker's key (`sv-voucher-client.jwks.json` from
  `make xaa-keys`) signed `NAME_XAA_CLIENT_ALG` (`PS256` or `RS256`, default
  `PS256`), for `NAME_XAA_ASSERTION_AUDIENCE` (`issuer`, the default, or
  `token_endpoint`).
- The token exchange grant for an ID-JAG (`requested_token_type`
  `urn:ietf:params:oauth:token-type:id-jag`) from the sign-in's ID token,
  audience Ledgerline's AS issuer, scope `research:read`, `resource`
  `https://mcp.ledgerline.lab/mcp`
- ID-JAGs issued under `NAME_ISSUER`, signed with keys at its `jwks_uri`,
  living 300 s at most, `client_id` S&V's client at Ledgerline's AS
- Ledgerline's AS trusting it as an ID-JAG issuer

After a change, run `make layer-45 layer-47 layer-80` (the broker releases
that IdP's ID token to `kagent`; the gateway's control plane fetches its
keys through `sv-egress`), then `./demos/bob/install.sh`.

### Directory

The directory sync gives the broker an account for each of the primary's
users with a verified email under `SV_EMAIL_DOMAINS`. It reads the primary's
groups and attributes and writes them to each failover that has a directory
([Directory sync](IDENTITY-CONTINUITY.md#directory-sync)). A primary's client
reads users and groups; a failover's also updates users.

| `NAME_DIRECTORY_TYPE` | API | Email verified from |
| --- | --- | --- |
| `scim` | SCIM 2.0 (`/Users`, `/Schemas`) | the email's `verified`, or an extension's `emailVerified` |
| `auth0` | Auth0 Management API v2 | `email_verified` |
| `keycloak` | Keycloak admin API, one realm | `emailVerified` |

## S&V's own Keycloak (`keycloak`)

`keycloak` in `ENTERPRISE_IDP` is a Keycloak S&V runs itself, apart from the
broker. Layer 45 installs it in `sv-workforce` at
`https://login.sterling.lab`, realm `workforce`, with the employees
(`bob` / `bob-demo`, `carol` / `carol-demo`, `dana` / `dana-demo`).

Sign-ins there take a password, then a one-time code from an authenticator
app (`acr aal2`). The employees' seeds are lab secrets
(`SV_WORKFORCE_TOTP_<USER>` in `.lab/secrets.env`). `make totp` prints Bob's
current code (`make totp EMPLOYEE=carol` for Carol's, `EMPLOYEE=dana` for
Dana's).

It is an upstream like the others. S&V's client there
(`sterling-vance-broker`) authenticates with the broker's key
(`private_key_jwt`). The directory sync reads and writes its users with
`continuity-directory` (`view-users`, `manage-users`) over its admin API,
which only the sync reaches. To use another Keycloak, set `KEYCLOAK_ISSUER`
and `KEYCLOAK_CLIENT_ID` in `.env` and register the callback and keys as for
any upstream.

## S&V's contingency IdP (`contingency`)

`contingency` in `ENTERPRISE_IDP` is the IdP S&V keeps for when its own is
down. Layer 45 installs a Keycloak in `sv-contingency` at
`https://login-dr.sterling.lab`, realm `contingency`, with its own accounts
for the employees (`bob` / `bob-demo`, `carol` / `carol-demo`, `dana` /
`dana-demo`) and a password only (`acr aal1`).

S&V's client there (`sterling-vance-broker`) takes the broker's key. Its
directory (`continuity-directory`, users' profiles only, never a credential)
is a failover in the directory sync, so the broker's users and their
attributes reach it before it's needed. Its own namespace keeps it up when
S&V's own Keycloak is cut; it is cut on its own (a DENY in `sv-contingency`).
Workloads that require more than a password fail closed while it signs
people in.

## Auth0 (`auth0`)

Set `AUTH0_ISSUER` in `.env` to your tenant's issuer, with its trailing slash
(`https://<tenant>.us.auth0.com/`). Unset, auth0 is left out of the chain.

1. **Applications → Create Application → Regular Web Application.** Settings:

   | Field | Value |
   | --- | --- |
   | Allowed Callback URLs | `https://idp.sterling.lab/realms/sterling-vance/broker/auth0/endpoint` |
   | Allowed Logout URLs | `https://idp.sterling.lab/realms/sterling-vance/broker/auth0/endpoint/logout_response` |
   | Credentials → Authentication Method | Client Secret (Post) |
   | Advanced → Grant Types | Authorization Code |

2. **Connections:** enable only Username-Password-Authentication. In
   Authentication → Database → Username-Password-Authentication, turn on
   **Disable Sign Ups**.

3. **Create the users** `bob@sterling.lab`, `carol@sterling.lab` and
   `dana@sterling.lab` with passwords of your choice, and mark their emails
   verified. Edit the email on the user's page, or `PATCH
   /api/v2/users/{id}` with `{"email_verified": true}` from the Management
   API Explorer. The directory sync gives S&V an account for each verified
   user under `sterling.lab` and refuses unverified emails.

4. **Roles, and the claim that carries them.** In User Management → Roles,
   create `advisors` (Bob), `platform-engineers` (Dana) and `compliance`
   (nobody, for the compliance-only tool). Auth0 puts roles in no token by
   itself. Add one with Actions → Library → Build Custom, trigger Login /
   Post Login, deployed into the Login flow:

   ```javascript
   exports.onExecutePostLogin = async (event, api) => {
     const roles = (event.authorization && event.authorization.roles) || [];
     api.idToken.setCustomClaim('https://sterling.lab/groups', roles);
   };
   ```

   The claim is namespaced because Auth0 drops custom claims that aren't.
   `AUTH0_GROUPS_CLAIM` defaults to this one.

5. **Give the lab the tenant and credentials** in `.env` (`auth0` is in the
   default `ENTERPRISE_IDP`), then install the layer again:

   ```
   AUTH0_ISSUER=https://<tenant>.us.auth0.com/
   AUTH0_CLIENT_ID=<Client ID>
   AUTH0_CLIENT_SECRET=<Client Secret>
   ```

   ```bash
   make layer-47
   ```

   This writes Secret `sv-identity/upstream-auth0`. Within three probes the
   IdP is healthy and, with automatic failback, active:

   ```bash
   kubectl --context kind-solo-lab get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}'
   ```

Bob's groups come from his Auth0 roles: from the claim at each sign-in, and
from the Management API in the directory sync.

**Assurance.** Auth0 puts `amr: ["mfa"]` in the ID token when the sign-in
took a second factor, which maps to AAL2. Without multi-factor
authentication (Security → Multi-factor Auth), Auth0 sign-ins prove AAL1, and
workloads that require more refuse them. If your tenant's policy always
takes a second factor for S&V's application, set
`tiers.auth0.assurance.default: AAL2` in `config/continuity.local.yaml` (the
example file shows it).

**Auth0 as a directory.** For the directory sync, create an application
(Applications → Create Application → Machine to Machine Applications)
authorized for the Auth0 Management API. Grant `read:users`, `update:users`,
`read:roles` and `read:role_members`. To write roles to Auth0 as a failover,
add `create:roles`, `create:role_members` and `delete:role_members`. The sync
needs nothing more; it never reads the tenant's settings. Put its
credentials in `.env`, then run `make layer-47`:

```
AUTH0_DIRECTORY_CLIENT_ID=<Client ID>
AUTH0_DIRECTORY_CLIENT_SECRET=<Client Secret>
```

This writes Secret `sv-identity/directory-auth0` and sets the auth0 IdP's
directory (`https://<tenant>/api/v2`). Map its attributes in the
Observatory's Directory sync.

## Gluu (`gluu`)

Gluu Flex or Janssen. It signs S&V's people in with a passkey by default
(`acr` `fido2`, AAL2 and phishing-resistant), and its SCIM API is its
directory.

```
GLUU_ISSUER=https://<S&V's Gluu>
GLUU_CLIENT_ID=<S&V's client there>
GLUU_DIRECTORY_CLIENT_ID=<the directory sync's client>
GLUU_DIRECTORY_CLIENT_SECRET=<its secret>
```

- **S&V's client:** as in [Registering S&V at an IdP](#registering-sv-at-an-idp),
  with `private_key_jwt`. Client assertions name Gluu by its issuer (`aud`),
  exactly.
- **Groups:** a `groups` claim in the ID token for S&V's client, with the
  user's Gluu groups by name.
- **Directory:** a client with the `client_credentials` grant and the scopes
  in `GLUU_DIRECTORY_SCOPES` (`https://jans.io/scim/users.read`,
  `https://jans.io/scim/groups.read`; add `https://jans.io/scim/users.write`
  for Gluu as a failover). Its SCIM API is `GLUU_DIRECTORY_URL`
  (`<issuer>/jans-scim/restv1/v2`).
- **Sign-in:** `acr_values=simple_password_auth` asks for a password
  instead of the passkey (AAL1).

For Gluu as Ledgerline's AS, see [below](#gluu-as-ledgerlines-as); for the
Gluu story, [GLUU.md](GLUU.md).

## Okta (`okta`)

```
OKTA_ISSUER=https://<org>.okta.com
OKTA_CLIENT_ID=<S&V's client there>
```

- **S&V's client:** an OIDC web app integration, client authentication with
  a public key, Authorization Code with PKCE required. Okta takes no PS256
  client assertions, so the broker signs ES256 (`OKTA_CLIENT_ALG`) with a
  key of its own: register `sv-upstream-client-es256.jwks.json`.
- **Groups:** a `groups` claim in the app's ID token (Sign On, OpenID Connect
  ID Token, groups claim filter), sent for scope `groups`
  (`OKTA_SCOPES`, default `openid email profile groups`).
- **Assurance:** `acr` from its authentication policies. `urn:okta:loa:2fa:any`
  is AAL2, `phr` phishing-resistant, `phrh` phishing-resistant and
  hardware-bound (AAL3). Without a requested `acr`, `amr` `mfa` is AAL2.
- **Directory:** none. Okta's Users API isn't SCIM, and the sync has no
  directory type for it. As the primary, Okta provisions no broker accounts;
  as a failover, it receives no attributes.

### Okta vouches (experimental)

`XAA_VOUCHER=okta` ([An IdP that vouches](#an-idp-that-vouches)). Okta's org
authorization server issues ID-JAGs (Cross App Access), so `OKTA_ISSUER` is
the org URL (`https://<org>.okta.com`, no `/oauth2/...` path), for sign-in
too: the ID-JAG's issuer, and that of the ID token it is made from.

```
XAA_VOUCHER=okta
OKTA_CLIENT_ID=<the AI agent's client ID>
```

- **AI agent** (Directory > AI Agents): S&V's requesting app. Registering
  it with **Create a new OIDC app linked to this AI agent** makes S&V's
  sign-in app, with the agent's client ID (`OKTA_CLIENT_ID`; then
  `XAA_CLIENT_ID` stays unset). Its public keys:
  `sv-upstream-client-es256.jwks.json` (the broker's sign-in) and
  `sv-voucher-client.jwks.json` (the gateway's ID-JAG requests, RS256,
  `OKTA_XAA_CLIENT_ALG`, for Okta's token endpoint,
  `OKTA_XAA_ASSERTION_AUDIENCE`). Redirect URI: the broker's
  (`https://idp.sterling.lab/realms/sterling-vance/broker/okta/endpoint`).
  - **Resource connections:** Ledgerline's app, with S&V's client ID at
    Ledgerline's AS (`LEDGERLINE_NAME_CLIENT_ID`, the ID-JAG's `client_id`),
    resource `https://mcp.ledgerline.lab/mcp` and scope `research:read`.
- **Ledgerline's app** (Applications, Machine Assignments > Callers > Cross
  App Access): the issuer of Ledgerline's AS, its tenant ID if it names one
  (sent as `aud_tenant`), and scope `research:read`.
- **Users** are assigned to both apps.

- **Authentication policy** of the agent's app: one its users can meet
  (Okta's default for new apps requires a phishing-resistant,
  device-bound factor).

ID-JAGs live 300 s, and their `sub` is the user's Okta ID.

## Ping Identity (`ping`)

```
PING_ISSUER=<its issuer, e.g. https://auth.pingone.com/<environment>/as>
PING_CLIENT_ID=<S&V's client there>
PING_DIRECTORY_URL=<its SCIM 2.0 endpoint>
PING_DIRECTORY_CLIENT_ID=<the directory sync's client>
PING_DIRECTORY_CLIENT_SECRET=<its secret>
```

- **S&V's client:** an OIDC web application with `private_key_jwt`
  (`sv-upstream-client.jwks.json`) or a client secret.
- **Groups:** the user's group names in a `groups` claim.
- **Assurance:** `amr` `mfa` is AAL2.

## Another OIDC IdP

Give it any name. Set its `NAME_*` settings in `.env`, add the name to
`ENTERPRISE_IDP`, then run `make layer-47`; re-running the layer keeps it.
An IdP added only in the Observatory's rule builder (**+ Add OIDC IdP**)
lasts until the layer is re-run.

## Ledgerline's authorization server

The same for every AS:

| Item | Value |
| --- | --- |
| Trusted ID-JAG issuer | the voucher only, by its keys: S&V's broker (`https://idp.sterling.lab/realms/sterling-vance`), or the IdP in `XAA_VOUCHER` |
| S&V's client | `LEDGERLINE_NAME_CLIENT_ID`, `private_key_jwt` RS256, JWKS `sv-ras-client.jwks.json` from `make xaa-keys`; or a client secret (`LEDGERLINE_NAME_CLIENT_SECRET`) at an AS that takes no client keys |
| Grant type | `urn:ietf:params:oauth:grant-type:jwt-bearer`; assertion reuse refused; assertion lifetime at most 300 s |
| Subject | the ID-JAG's `sub` (the user's broker account) mapped to Ledgerline's user, or its verified `email` |
| Access token | JWT; `aud` `ledgerline-research`; `scope` `research:read` when requested (a space-separated string, a list, or a list written as a string); `client_id` S&V's client; `user_name` or `email`; living no longer than the ID-JAG it was issued for (`MCP_TOKEN_LIFETIME`); no refresh token |

Ledgerline's MCP server and gateway accept tokens from that issuer only. The
server requires scope `research:read` from S&V's client and fetches the AS's
keys through `ledgerline-egress`.

S&V's broker is reachable only inside the lab. Until the lab is hosted with
a public issuer, an AS outside it trusts the broker by its public keys
(`sv-idp.jwks.json` from `make xaa-keys`), published where the AS can fetch
them.

### Ledgerline's Keycloak (`keycloak`)

The default: `https://idp.ledgerline.lab/realms/ledgerline`, which the lab
runs. It trusts S&V's broker, for S&V's domain only, and links Bob's seat
to the broker account the directory sync made for him.

### Gluu as Ledgerline's AS

```
RESOURCE_AS=gluu
LEDGERLINE_GLUU_ISSUER=https://<Ledgerline's Gluu>
LEDGERLINE_GLUU_CLIENT_ID=<S&V's client there>
```

Its trusted issuer is S&V's broker, by `sv-idp.jwks.json`, or with
`XAA_VOUCHER=gluu` S&V's Gluu, by its `jwks_uri`.

### Auth0 as Ledgerline's AS (experimental)

Auth0 redeems ID-JAGs from an Okta that vouches (`XAA_VOUCHER=okta`), in a
tenant of Ledgerline's own (not S&V's Auth0).

```
RESOURCE_AS=auth0
LEDGERLINE_AUTH0_ISSUER=https://<Ledgerline's tenant>/
LEDGERLINE_AUTH0_CLIENT_ID=<S&V's client there>
```

- **Trust in Okta:** an Okta Workforce enterprise connection to Okta's
  Ledgerline app, with Cross App Access as the resource application. Auth0
  doesn't create users from an ID-JAG: each user signs in through that
  connection once first.
- **S&V's client:** Cross App Access on, `private_key_jwt` with
  `sv-ras-client.jwks.json`, enabled on that connection.
- **API:** identifier `https://mcp.ledgerline.lab/mcp`
  (`LEDGERLINE_AUTH0_AUDIENCE`), the resource the ID-JAG names; permission
  `research:read`; token expiration 300 s.

The issuer ends in `/`, as Auth0's discovery states it.

### Descope as Ledgerline's AS (experimental)

Descope redeems ID-JAGs from any IdP it trusts (Okta, with
`XAA_VOUCHER=okta`, or another).

```
RESOURCE_AS=descope
LEDGERLINE_DESCOPE_ISSUER=https://api.descope.com/v1/apps/agentic/<project>/<resource>
LEDGERLINE_DESCOPE_CLIENT_ID=<S&V's client there>
LEDGERLINE_DESCOPE_CLIENT_SECRET=<its secret>
```

- **Resource:** type MCP server, URI `https://mcp.ledgerline.lab/mcp`
  (`LEDGERLINE_DESCOPE_AUDIENCE`), scope `research:read`. Its issuer is
  `LEDGERLINE_DESCOPE_ISSUER`, the ID-JAG's audience.
- **Tenant** (Authentication Methods, SSO, Cross-App Access): ID-JAGs
  allowed, the vouching IdP's issuer trusted, JIT provisioning on. Its
  tenant ID is the ID-JAG's `aud_tenant`.
- **S&V's client:** a client of that resource. Descope's token endpoint
  takes client secrets only.
- **Policy:** a rule granting that client `research:read` on the resource
  (Machine-to-Machine and Delegated access). Without one, Descope grants
  only `profile email`.

The ID-JAG names the resource's issuer, not the project-level audience the
tenant page shows.

### Okta to Descope, end to end

1. `make xaa-keys`, then register in Okta, on the AI agent:
   `sv-upstream-client-es256.jwks.json` and `sv-voucher-client.jwks.json`
   (with `XAA_VOUCHER=okta` in `.env`, so it is written).
2. In Descope: the resource, the tenant's Cross-App Access (Okta's issuer
   trusted), S&V's client and the policy, as above.
3. In Okta: the AI agent and its linked app (redirect URI, groups claim,
   authentication policy), Ledgerline's app with Cross App Access (the
   resource's issuer, the tenant ID, `research:read`), the resource
   connection (S&V's client ID at Descope), users assigned to both apps.
4. `.env` (`.env.example` lists the settings), then
   `make layer-45 layer-47 layer-80` and `./demos/bob/install.sh`.
5. Sign in at `https://kagent.sterling.lab` through Okta and ask Bob's
   assistant which Ledgerline account he is using; `make xaa-logs` has the
   trail.

### Another AS

Set `RESOURCE_AS=<name>`, `LEDGERLINE_<NAME>_ISSUER` and
`LEDGERLINE_<NAME>_CLIENT_ID`, and register it as above.
