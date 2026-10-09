# IdPs and authorization servers

Two sets of identity providers, both chosen by name in `.env` and configured
the same way:

- **S&V's IdPs** (`ENTERPRISE_IDP`, in failover order; the first is the
  primary): who signs S&V's people in, and who vouches for them to other
  companies (the ID-JAG for Cross App Access). S&V's broker routes every
  sign-in to the active one and maps it into one shape
  ([IDENTITY-CONTINUITY.md](IDENTITY-CONTINUITY.md)).
- **Ledgerline's authorization server** (`RESOURCE_AS`): redeems S&V's
  ID-JAGs for Ledgerline access tokens
  ([IDENTITY-FLOWS.md](IDENTITY-FLOWS.md#2-cross-app-access-id-jag-to-a-saas)).

Any combination works. A fresh clone runs `keycloak,contingency` (S&V's own
two, in the lab) and Ledgerline's own Keycloak; nothing outside the lab is
needed. An IdP without an issuer is left out of the chain.

| `ENTERPRISE_IDP` | `RESOURCE_AS` | Bob signs in at | Vouches for Bob | Redeems |
| --- | --- | --- | --- | --- |
| `keycloak,contingency` (a fresh clone) | `keycloak` | S&V's Keycloak | S&V's Keycloak | Ledgerline's Keycloak |
| `auth0,keycloak,contingency` | `keycloak` | Auth0 | S&V's broker | Ledgerline's Keycloak |
| `gluu,auth0,keycloak,contingency` | `gluu` | Gluu (passkey) | Gluu | Ledgerline's Gluu |
| `okta,keycloak` | `keycloak` | Okta | S&V's broker | Ledgerline's Keycloak |
| `ping,keycloak,contingency` | `keycloak` | Ping | S&V's broker | Ledgerline's Keycloak |

After a failover Bob signs in at the next IdP, and that IdP (or, if it
doesn't issue ID-JAGs, S&V's broker) vouches.

## Settings

S&V's IdPs, by name (`NAME` is the name in upper case). Set the first three
in `.env`; `config/lab.env` has the rest for the IdPs below, and `.env`
overrides any of them. Changing them: `make layer-47`, then
`./demos/bob/install.sh` for Cross App Access.

| Setting | |
| --- | --- |
| `NAME_ISSUER` | its issuer, exactly as its discovery states it |
| `NAME_CLIENT_ID` | S&V's client there |
| `NAME_CLIENT_SECRET` | unset: S&V authenticates with its keys (`private_key_jwt`); set: `client_secret_post` |
| `NAME_DISPLAY_NAME` | its name on sign-in pages and in the Observatory |
| `NAME_GROUPS_CLAIM` | the ID token claim with the user's groups (or roles); default `groups` |
| `NAME_ISSUES_IDJAG` | `true`: it vouches for its users itself; else S&V's broker vouches for its sign-ins |
| `NAME_ASSURANCE` | what its sign-ins prove: NIST 800-63B levels by the `acr` or `amr` it asserts (JSON, [assurance](IDENTITY-CONTINUITY.md#assurance)); unset: AAL1 |
| `NAME_LEDGERLINE_CLIENT_ID` | Ledgerline's SSO client there; default `ledgerline` |
| `NAME_DIRECTORY_TYPE` | the directory sync's access to its users: `scim`, `auth0` or `keycloak`; unset: none |
| `NAME_DIRECTORY_URL` | the directory's API |
| `NAME_DIRECTORY_CLIENT_ID`, `NAME_DIRECTORY_CLIENT_SECRET` | a `client_credentials` client allowed to read users (and, for a failover, update them) |
| `NAME_DIRECTORY_SCOPES`, `NAME_DIRECTORY_AUDIENCE` | what that client asks for |

Ledgerline's authorization server, by its name in `RESOURCE_AS`:

| Setting | |
| --- | --- |
| `LEDGERLINE_NAME_ISSUER` | its issuer, exactly as its discovery states it. One under `.lab` is Ledgerline's own Keycloak, which the lab runs |
| `LEDGERLINE_NAME_CLIENT_ID` | S&V's client there; default `sterling-vance-kagent` |

An ID-JAG's `client_id` must name S&V's client at Ledgerline's AS, so every
IdP that vouches for S&V puts the same value there. An IdP that puts the
requesting client's own ID (Gluu) needs S&V registered at the AS under that
ID: set `LEDGERLINE_NAME_CLIENT_ID` to it, and S&V's Keycloak and broker put
it in their ID-JAGs too. Ledgerline's AS must be a different deployment from
any IdP in the chain: an IdP can't vouch for Bob to itself.

## Registering S&V at an IdP

The same for every IdP:

| Item | Value |
| --- | --- |
| Application | confidential OIDC web application |
| Redirect URI | `https://idp.sterling.lab/realms/sterling-vance/broker/<name>/endpoint` (also in `status.tiers[].redirectURI`) |
| Logout redirect | `https://idp.sterling.lab/realms/sterling-vance/broker/<name>/endpoint/logout_response` |
| Client authentication | `private_key_jwt` with the JWKS `sv-upstream-client.jwks.json` from `make xaa-keys` (the broker's key, PS256, and the egress's key, RS256), or a client secret (`client_secret_post`) |
| Grant types | `authorization_code` with PKCE S256, `refresh_token` |
| Scopes | `openid email profile` |
| Users | `email` and `email_verified: true` in the ID token: the broker links a sign-in to the S&V account with that verified email |
| Groups | the user's groups by name in `NAME_GROUPS_CLAIM`, named as in `SV_GROUPS` (`advisors`, `platform-engineers`, `compliance`) |

The `.lab` hosts resolve only on your machine. That's fine: an IdP only
redirects the browser to them, and every call S&V makes to it goes out from
the lab.

An IdP that vouches for its users (`NAME_ISSUES_IDJAG=true`) also allows
S&V's client:

| Item | Value |
| --- | --- |
| Grant type | `urn:ietf:params:oauth:grant-type:token-exchange` |
| Refresh tokens | bound to the user's session at the IdP (no offline access), not rotated (a confidential client authenticating with a key; RFC 9700 4.14.2) |
| Token exchange | the user's access token for their ID token (`requested_token_type` `urn:ietf:params:oauth:token-type:id_token`); that ID token for an ID-JAG (`requested_token_type` `urn:ietf:params:oauth:token-type:id-jag`, `audience` Ledgerline's AS issuer) |
| ID-JAG | `typ` `oauth-id-jag+jwt`; `aud` Ledgerline's AS issuer; `client_id` S&V's client at Ledgerline's AS; `sub`, `iat`; at most 300 s |
| Ledgerline's SSO client | `NAME_LEDGERLINE_CLIENT_ID`, `private_key_jwt` with `ledgerline-sso-client.jwks.json`; redirect `https://idp.ledgerline.lab/realms/ledgerline/broker/sterling-vance-<name>/endpoint`; PKCE S256; `openid email profile` (Ledgerline's own Keycloak links Bob's seat at his first sign-in through the IdP) |

### Directory

The directory sync gives the broker an account for each of the primary's
users with a verified email under `SV_EMAIL_DOMAINS`, reads the primary's
groups and attributes, and writes them to each failover that has a directory
([Directory sync](IDENTITY-CONTINUITY.md#directory-sync)). A primary's client
reads users and groups; a failover's also updates users.

| `NAME_DIRECTORY_TYPE` | API | Email verified from |
| --- | --- | --- |
| `scim` | SCIM 2.0 (`/Users`, `/Schemas`) | the email's `verified`, or an extension's `emailVerified` |
| `auth0` | Auth0 Management API v2 | `email_verified` |
| `keycloak` | Keycloak admin API, one realm | `emailVerified` |

## S&V's own Keycloak (`keycloak`)

`keycloak` in `ENTERPRISE_IDP` is a Keycloak S&V runs itself, apart from the
broker: layer 45 installs one in `sv-workforce` at
`https://login.sterling.lab`, realm `workforce`, with the employees
(`bob` / `bob-demo`, `carol` / `carol-demo`, `dana` / `dana-demo`). The broker's sign-ins there
take a password and then a one-time code from an authenticator app (`acr
aal2`). The employees' seeds are lab secrets (`SV_WORKFORCE_TOTP_<USER>` in
`.lab/secrets.env`); `make totp` prints Bob's current code
(`make totp EMPLOYEE=carol` for Carol's, `EMPLOYEE=dana` for Dana's). It is an upstream like the
others: S&V's client there (`sterling-vance-broker`) authenticates with the
broker's key and the egress's key (`private_key_jwt`), it issues ID-JAGs for
the broker's sign-ins, and the directory sync reads and writes its users
with `continuity-directory` (`view-users`, `manage-users`) over its admin
API, which only the sync reaches. To use another Keycloak, set `KEYCLOAK_ISSUER` and
`KEYCLOAK_CLIENT_ID` in `.env` and register the callback and keys as for any
upstream.

## S&V's contingency IdP (`contingency`)

`contingency` in `ENTERPRISE_IDP` is the IdP S&V keeps for when its own is
down: layer 45 installs a Keycloak in `sv-contingency` at
`https://login-dr.sterling.lab`, realm `contingency`, with its own accounts
for the employees (`bob` / `bob-demo`, `carol` / `carol-demo`, `dana` / `dana-demo`) and a password
only (`acr aal1`). S&V's client there (`sterling-vance-broker`) takes the
broker's key, and its directory (`continuity-directory`, users' profiles only,
never a credential) is wired into the directory sync as a failover, so the
broker's users and their attributes reach it before it's needed. It is in its own namespace, so cutting S&V's own Keycloak
leaves it up, and it is cut on its own (a DENY in `sv-contingency`). It
doesn't issue ID-JAGs: the broker vouches for its sign-ins. Workloads that
require more than a password fail closed while it signs people in.

## Auth0 (`auth0`)

Tenant: `AUTH0_ISSUER` in `.env`, your tenant's issuer with its trailing
slash (`https://<tenant>.us.auth0.com/`). Unset, auth0 is left out of the chain.

1. **Applications → Create Application → Regular Web Application.** Settings:

   | Field | Value |
   | --- | --- |
   | Allowed Callback URLs | `https://idp.sterling.lab/realms/sterling-vance/broker/auth0/endpoint` |
   | Allowed Logout URLs | `https://idp.sterling.lab/realms/sterling-vance/broker/auth0/endpoint/logout_response` |
   | Credentials → Authentication Method | Client Secret (Post) |
   | Advanced → Grant Types | Authorization Code |

   The `.lab` hosts only resolve on your machine. That's fine: Auth0 only
   redirects the browser to them.

2. **Connections:** enable only Username-Password-Authentication. In
   Authentication → Database → Username-Password-Authentication, turn on
   **Disable Sign Ups**.

3. **Create the users** `bob@sterling.lab`, `carol@sterling.lab` and
   `dana@sterling.lab` with passwords of your choice, and **mark their emails
   verified** (edit the email on the user's page, or `PATCH
   /api/v2/users/{id}` with `{"email_verified": true}` from the Management
   API Explorer). The directory sync gives S&V an account for each verified
   user under `sterling.lab`; unverified emails are refused.

4. **Roles, and the claim that carries them.** User Management → Roles:
   `advisors` (Bob), `platform-engineers` (Dana) and `compliance` (nobody, for
   the compliance-only tool), with those names. Auth0 puts roles in no token
   by itself: Actions → Library → Build Custom, trigger Login / Post Login,
   deployed into the Login flow:

   ```javascript
   exports.onExecutePostLogin = async (event, api) => {
     const roles = (event.authorization && event.authorization.roles) || [];
     api.idToken.setCustomClaim('https://sterling.lab/groups', roles);
   };
   ```

   The claim is namespaced because Auth0 drops custom claims that aren't
   (`AUTH0_GROUPS_CLAIM`, this one by default).

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

Bob's groups come from his Auth0 roles: at each sign-in from the claim, and
in the directory sync from the Management API.

**Assurance:** Auth0 puts `amr: ["mfa"]` in the ID token when the sign-in
took a second factor, which maps to AAL2. Without multi-factor
authentication (Security → Multi-factor Auth), Auth0 sign-ins prove AAL1 and
workloads that require more refuse them. If your tenant's policy always
takes a second factor for S&V's application, say so in
`config/continuity.local.yaml` (`tiers.auth0.assurance.default: AAL2`; the
example file shows it).

**Auth0 as a directory** (the directory sync): **Applications → Create
Application → Machine to Machine Applications**, authorized for the **Auth0
Management API** with `read:users`, `update:users`, `read:roles`,
`read:role_members` and, to write roles to Auth0 as a failover,
`create:roles` and `create:role_members`, `delete:role_members`. Nothing
more: the sync never needs the tenant's settings. Its credentials in
`.env`, then `make layer-47`:

```
AUTH0_DIRECTORY_CLIENT_ID=<Client ID>
AUTH0_DIRECTORY_CLIENT_SECRET=<Client Secret>
```

This writes Secret `sv-identity/directory-auth0` and sets the auth0 IdP's
directory (`https://<tenant>/api/v2`); map its attributes in the
Observatory's Directory sync.

## Gluu (`gluu`)

Gluu Flex or Janssen. It vouches for its users (ID-JAGs), signs them in with a
passkey by default (`acr` `fido2`, AAL2 and phishing-resistant), and its SCIM
API is its directory.

```
GLUU_ISSUER=https://<S&V's Gluu>
GLUU_CLIENT_ID=<S&V's client there>
GLUU_DIRECTORY_CLIENT_ID=<the directory sync's client>
GLUU_DIRECTORY_CLIENT_SECRET=<its secret>
```

- **S&V's client:** as in [Registering S&V at an IdP](#registering-sv-at-an-idp),
  with `private_key_jwt`: client assertions name Gluu by its issuer (`aud`),
  exactly. Janssen puts this client's ID in the ID-JAGs it issues, so S&V
  registers at Ledgerline's AS under it (`LEDGERLINE_NAME_CLIENT_ID`).
- **Groups:** a `groups` claim in the ID token for S&V's client, with the
  user's Gluu groups by name.
- **Directory:** a client with the `client_credentials` grant and the scopes
  in `GLUU_DIRECTORY_SCOPES` (`https://jans.io/scim/users.read`,
  `https://jans.io/scim/groups.read`; add `https://jans.io/scim/users.write`
  for Gluu as a failover). Its SCIM API is `GLUU_DIRECTORY_URL`
  (`<issuer>/jans-scim/restv1/v2`).
- **Sign-in:** `acr_values=simple_password_auth` asks for a password
  instead of the passkey (AAL1).

Gluu as Ledgerline's AS: [below](#gluu-as-ledgerlines-as). The Gluu story:
[GLUU.md](GLUU.md).

## Okta (`okta`)

```
OKTA_ISSUER=https://<org>.okta.com
OKTA_CLIENT_ID=<S&V's client there>
```

- **S&V's client:** an OIDC web app integration, client authentication with
  a public key (`sv-upstream-client.jwks.json`), Authorization Code with PKCE
  required.
- **Groups:** a `groups` claim on the authorization server, filtered to S&V's
  groups.
- **Assurance:** `acr` from its authentication policies: `urn:okta:loa:2fa:any`
  AAL2, `phr` phishing-resistant, `phrh` phishing-resistant and hardware-bound
  (AAL3).
- **Vouches:** the broker vouches for Okta sign-ins.
- **Directory:** none. Okta's Users API isn't SCIM, and the sync has no
  directory type for it: Okta as the primary provisions no broker accounts,
  and as a failover receives no attributes.

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
- **Vouches:** the broker vouches for Ping sign-ins.

## Another OIDC IdP

Any name: its `NAME_*` settings in `.env` and its name in `ENTERPRISE_IDP`,
then `make layer-47`. Re-running the layer keeps it. An IdP added only in the
Observatory's rule builder (**+ Add OIDC IdP**) lasts until the layer is
re-run.

## Ledgerline's authorization server

The same for every AS:

| Item | Value |
| --- | --- |
| Trusted ID-JAG issuers | each of S&V's IdPs that vouch for its users, and S&V's broker (`https://idp.sterling.lab/realms/sterling-vance`), each by its keys |
| S&V's client | `LEDGERLINE_NAME_CLIENT_ID`, `private_key_jwt` RS256, JWKS `sv-ras-client.jwks.json` from `make xaa-keys` |
| Grant type | `urn:ietf:params:oauth:grant-type:jwt-bearer`; assertion reuse refused; assertion lifetime at most 300 s |
| Subject | the ID-JAG's `sub` per issuer mapped to Ledgerline's user, or its verified `email` |
| Access token | JWT; `aud` `ledgerline-research`; `scope` `research:read` when requested; `client_id` S&V's client; `user_name` or `email`; at most 300 s |

Ledgerline's MCP server and gateway accept tokens from that issuer only; the
server requires scope `research:read` from S&V's client, and fetches the AS's
keys through `ledgerline-egress`.

S&V's own issuers are reachable only inside the lab. An AS outside it trusts
them by their public keys: `sv-idp.jwks.json` (S&V's broker) and
`sv-workforce.jwks.json` (S&V's Keycloak) from `make xaa-keys`, published
where the AS can fetch them, or the lab hosted with public issuers. Until
then only an IdP outside the lab can vouch to it.

### Ledgerline's Keycloak (`keycloak`)

The default: `https://idp.ledgerline.lab/realms/ledgerline`, which the lab
runs. It trusts each of S&V's IdPs that vouch, for S&V's domain only, and
links Bob's seat to the broker account the directory sync made for him.

### Gluu as Ledgerline's AS

```
RESOURCE_AS=gluu
LEDGERLINE_GLUU_ISSUER=https://<Ledgerline's Gluu>
LEDGERLINE_GLUU_CLIENT_ID=<GLUU_CLIENT_ID, with S&V's Gluu vouching>
```

Its trusted issuers name each IdP that vouches (with S&V's Gluu, its
`jwks_uri`).

### Another AS

Any name: `RESOURCE_AS=<name>`, `LEDGERLINE_<NAME>_ISSUER` and
`LEDGERLINE_<NAME>_CLIENT_ID`, registered as above.
