# Identity continuity

Sterling & Vance's broker (Keycloak at `https://idp.sterling.lab`, realm
`sterling-vance`) routes workforce sign-in to an ordered chain of the firm's
IdPs (`ENTERPRISE_IDP`: Okta, Auth0, Gluu, the Keycloak S&V runs itself, or
its password-only contingency IdP) and maps each into one profile. The broker is not an IdP for the workforce:
it holds no employee passwords, only break-glass accounts for platform admins.
S&V's own services trust only the broker. When an IdP goes down, new sign-ins
move to the next healthy one and nothing downstream changes: same issuer,
same `sub`, same groups. The broker also vouches for S&V's users to other
companies (Cross App Access), whichever IdP signed them in, so partners
trust one issuer and never see the chain or its failover
([IDENTITY-FLOWS.md](IDENTITY-FLOWS.md#2-cross-app-access-id-jag-to-a-saas)).

Failover keeps people signed in; it must not lower what a sign-in proves.
The broker carries how the upstream authenticated each sign-in (its `acr`,
`amr` and `auth_time`), each IdP declares what its assertions are worth
([assurance](#assurance)), and each group of workloads says what it requires
when identity fails over ([assurance rules](#assurance-rules)): how
critical it is, which IdPs may vouch for its users, and the assurance level a
sign-in must prove. The assurance gate enforces that at the workloads'
gateways and refuses what can't meet it. The broker never steps anyone up
itself: a stronger sign-in happens at the user's IdP, or the request fails
closed. The controller also checks that every IdP in the chain still accepts
S&V's registration the way the broker uses it
([trust across IdPs](#trust-across-idps)).

[![Identity continuity at 4x speed: the upstream IdP signing people in, a simulated outage at the firm's egress, failover to the next IdP, and failback](videos/identity-continuity.gif)](videos/identity-continuity.mp4)

- API and controller: `apps/continuity` (`IdentityContinuity` and `WorkloadProfile`, `continuity.lab.solo.io/v1alpha1`; the assurance gate is its `gate` command)
- Install: `platform/47-continuity` (`make layer-47`, after `45-identity`)
- Demo cards: [cards/identity-continuity.html](cards/identity-continuity.html) (failover), [cards/assurance.html](cards/assurance.html) (assurance rules)
- Checks: `make continuity-verify`

```bash
kubectl --context kind-solo-lab get idc,wlp -n sv-identity
```

## The resource

The chain comes from `ENTERPRISE_IDP` in `config/lab.env` (default
`auth0,keycloak,contingency`), in any order, then `break-glass`, the broker's
own accounts for platform admins. An IdP without `<NAME>_ISSUER` in `.env` is
left out, so with no Auth0 tenant the lab signs people in through S&V's own
Keycloak, then its contingency IdP. Failback is automatic
(`platform/47-continuity/identitycontinuity.yaml`).

Each IdP is configured by its name: `<NAME>_ISSUER`, `<NAME>_CLIENT_ID` and
the rest of its settings ([IDPS.md](IDPS.md#settings)). `keycloak` is the
Keycloak S&V runs itself (`https://login.sterling.lab`, realm `workforce`:
password and a one-time code), `contingency` its password-only IdP for when
that one is down (`https://login-dr.sterling.lab`); both come with the lab.

Every IdP in the chain is trusted for S&V's workforce: a user who signs in
through it is linked to the S&V account with the same verified email. The
broker is the shape every IdP maps into, not a directory of its own: the
directory sync gives it an account for each of the primary's users under the
workforce's email domains (`spec.profile.domains`), without a password, and
it never creates one at sign-in. Their groups come from the IdPs too
([Groups](#groups)). Chain
only IdPs that are authoritative for S&V's users. Accounts with role
`local-only` (the `platform-admins` group: break-glass and platform admins)
are never linked to an upstream; with every IdP down they are the only ones
who can sign in. Only the active IdP is enabled at the broker; the others
keep their users' links but can't sign anyone in, not even by `kc_idp_hint`.

S&V's broker authenticates to an upstream with `private_key_jwt`
(`tiers[].oidc.clientAuth`), signing with a PS256 realm key kept for that alone; its
public half is in the realm's JWKS and in `make xaa-keys`
(`sv-upstream-client.jwks.json`). An upstream given `<NAME>_CLIENT_SECRET` in
`.env` uses `client_secret_post` instead ([Auth0](IDPS.md#auth0-auth0)).

Each upstream's ServiceEntry is exported to `sv-identity`, so every S&V
call to an upstream leaves through `sv-egress/egress-waypoint`, and a
partition there cuts all of them. Cross App Access calls no upstream: the
broker vouches, and the assurance gate refuses a session from an IdP that is
no longer active.

Re-running the layer sets the chain to `ENTERPRISE_IDP`: an IdP added in
the rule builder or by hand is removed. For each IdP it keeps what operators
set (display name, enabled, drain, failover rules, attributes, directory);
`.env` sets issuers, client IDs, client authentication and token settings.
`spec.profile` and `spec.sync` are left as they are.

| `ENTERPRISE_IDP` | Sign-in |
| --- | --- |
| `keycloak` | S&V's own Keycloak |
| `auth0,keycloak,contingency` (default) | Auth0, else S&V's own Keycloak, else its contingency IdP |
| `keycloak,contingency` | S&V's own Keycloak, else its contingency IdP (the default without an Auth0 tenant) |
| `okta,auth0,keycloak` | Okta, else Auth0, else S&V's own Keycloak |
| `keycloak,auth0` | S&V's own Keycloak, else Auth0 |
| `gluu,auth0,keycloak,contingency` | Gluu, else Auth0, else S&V's own Keycloak, else its contingency IdP ([IDPS.md](IDPS.md#gluu-gluu)) |

`make verify` and `make tour` sign Bob in through S&V's own Keycloak
(password, then a one-time code they compute from his seed), so `keycloak`
must be in `ENTERPRISE_IDP`; for their run they drain the IdPs ahead of it
and restore the chain on exit. The failover checks for assurance rules
need `contingency` after it.

### What the broker realm must already have

The controller manages identity providers and one redirector setting. It
doesn't create flows or roles, so the realm needs them first
(`platform/45-identity/realm-sterling-vance.json` has all of them):

- a browser flow (`continuity-browser`) with an **Identity Provider
  Redirector** execution, bound as the realm's browser flow;
- a first-broker-login flow (`continuity-first-broker-login`) that detects
  the existing user and links automatically (`idp-detect-existing-broker-user`,
  `idp-auto-link`);
- a service-account client whose roles include realm-management
  `manage-identity-providers` and `manage-realm` (editing the redirector's
  config is realm config; so is the user profile), with its credentials in
  the Secret `broker.keycloak.credentialsRef` names;
- for `sync`: a second service-account client with realm-management
  `view-users` and `manage-users` only (`continuity-sync`), its credentials
  in the Secret `sync.credentialsRef` names;
- for assurance rules: client scope `continuity-assurance`, which puts the
  session notes `continuity.acr`, `continuity.amr` and `continuity.auth_time`
  in tokens as `idp_acr`, `idp_amr` and `idp_auth_time`, on the clients users
  sign in through; and `view-clients` on the controller's client, to read
  those clients' registrations;
- the identity provider mapper `continuity-session-claims-idp-mapper`
  (a provider in the broker's image, `tools/keycloak-idjag/session-claims`).

### spec

| Field | Meaning |
| --- | --- |
| `broker.keycloak.url` | in-cluster Keycloak base URL (admin API) |
| `broker.keycloak.realm` | realm to manage |
| `broker.keycloak.credentialsRef` | Secret with `client-id`/`client-secret` of a service-account client with realm-management `manage-identity-providers` and `manage-realm` |
| `broker.keycloak.browserFlow` | browser flow whose Identity Provider Redirector points at the active tier (`continuity-browser`) |
| `broker.keycloak.firstBrokerLoginFlow` | first-broker-login flow for every upstream (`continuity-first-broker-login`: link to the existing user) |
| `egress.namespace`, `egress.waypoint` | route back-channel calls to external upstreams through this waypoint (one ServiceEntry per external IdP) |
| `egress.internalDomains` | hosts under these domains are in-cluster and get no ServiceEntry |
| `egress.exportTo` | other namespaces that call upstreams for the broker: the ServiceEntries are exported there too |
| `tiers[]` | ordered; the first eligible, healthy tier is active |
| `tiers[].name` | also the Keycloak IdP alias |
| `tiers[].displayName` | shown on the login page and in the Observatory |
| `tiers[].type` | `oidc` (an IdP) or `local` (the broker's break-glass accounts) |
| `tiers[].enabled` | `false`: never active, Keycloak IdP disabled |
| `tiers[].drain` | out of rotation; sessions in flight keep working |
| `tiers[].oidc.issuer` | exactly as the upstream publishes it (Auth0's ends in `/`) |
| `tiers[].oidc.clientID` | falls back to the Secret's `client-id` key; required with `private_key_jwt` |
| `tiers[].oidc.clientAuth` | `client_secret_post` (default), `client_secret_basic` or `private_key_jwt` |
| `tiers[].oidc.clientAssertionSigningAlg` | `PS256`: the realm key kept for client assertions |
| `tiers[].oidc.clientSecretRef` | Secret and key (default `client-secret`); required unless `clientAuth` is `private_key_jwt`; missing means `NotConfigured` |
| `tiers[].oidc.scopes` | default `openid email profile` |
| `tiers[].oidc.storeTokens` | keep the IdP's tokens on each user's link |
| `tiers[].attributes[]` | the IdP's attributes paired with S&V's profile for the directory sync: `attribute` (built-in or `profile.attributes`) and `path` (the attribute in the IdP's user record) |
| `tiers[].directory` | where the sync reads and writes this IdP's users: `type` (`scim`, `auth0`, `keycloak`), `url`, `credentialsRef` (`client-secret`, and `client-id` unless `clientID` is set), `scopes`, `audience` (Auth0) |
| `tiers[].directory.clientID` | the directory client's ID; falls back to the Secret's `client-id` key |
| `tiers[].assurance.levels[]` | what the IdP's sign-ins prove: an `acr` value, or one `amr` value, mapped to a NIST SP 800-63B `level` (`AAL1`, `AAL2`, `AAL3`), and whether that authenticator is `phishingResistant` |
| `tiers[].assurance.default` | the level of a sign-in that asserts none of them (default `AAL1`); for a local tier, its only level |
| `tiers[].failoverWhen` | which probe results count against the tier: `unreachable`, `serverError`, `invalidDiscovery` (each default true), `latencyAboveMs` (must be below `health.timeoutSeconds`: a slower answer times out first) |
| `health` | `intervalSeconds`, `timeoutSeconds`, `unhealthyThreshold` (failures in a row to go down), `healthyThreshold` (successes in a row to come back) |
| `failback` | `Automatic` (move back up as soon as a higher tier is healthy) or `Manual` |
| `profile.attributes[]` | S&V's profile beyond `username`, `email`, `firstName`, `lastName`: `name`, `displayName`, `type` (`string` default, `integer`, `number`, `boolean`, `date`, `email`, `uri`: checked by the broker), `multivalued` (a list) |
| `sync.schedule` | cron, UTC; `sync.suspend` pauses it |
| `sync.credentialsRef` | Secret with the sync's realm client (default `continuity-sync`) |

### status

`active` (where logins go now: the tier Keycloak's redirector points at, or
the local tier), `activeSince`, `broker.issuer`, `egressNamespace`, per tier (`configured`, `healthy`,
`partitioned`, `latencyMs`, `reason`, `message`, consecutive failures and
successes, `redirectURI`: the callback the upstream app must allow, and
`trust`: its [trust checks](#trust-across-idps)),
the last 20 `transitions`, `sync` (CronJob, last run and last success,
users, S&V profiles updated, failover accounts written and created,
failures, and each IdP's attribute paths as its directory last showed
them), and
conditions `Ready`, `Degraded` (not on the first IdP), `RulesEffective`
(False while a latency rule is at or above the probe timeout),
`ProfileApplied` and `TrustConsistent` (False while an IdP fails a trust
check). Events: `TierHealthy`, `TierUnhealthy`, `FailoverActivated`,
`Failback`, `TrustMismatch`, `CleanupAbandoned`.

## What the controller does

Every `health.intervalSeconds`, two replicas, one leader (Lease
`continuity.lab.solo.io` in `sv-identity`):

1. **Probe each tier.** `oidc`: fetch discovery and JWKS within
   `timeoutSeconds`; every endpoint in the discovery document must be https.
   Results: `Healthy`, `Unreachable`, `ServerError`, `InvalidDiscovery`,
   `SlowResponse`. `local`: healthy while Keycloak is reachable. A tier
   without credentials is probed but `NotConfigured`. A probe counts toward
   the thresholds at most once per interval, so editing the spec doesn't
   speed up a failover.
2. **Pick the active tier:** the first that is enabled, not drained,
   configured and healthy. With `failback: Manual` it stays put until the
   current tier fails. With nothing eligible, the first local tier that is
   enabled and not drained. While Keycloak itself is unreachable nothing can
   change, so the active tier holds.
3. **Make Keycloak match**, through its admin API:
   - one OIDC identity provider per configured `oidc` tier (hidden from the
     login page when not eligible), with PKCE S256, the tier's
     `oidc.clientAuth` (`private_key_jwt` with the broker's PS256 key, or a
     client secret), and a claim filter that only accepts
     `email_verified: true`. Only the active IdP is enabled; the others keep
     their users' links, disabled. A setting
     changed by hand in Keycloak is put back. If a tier's Secret goes
     missing its IdP stays (hidden), so users' links to it survive; only
     removing the tier from the spec deletes it;
   - the `continuity-browser` flow's IdP redirector set to the active tier,
     or cleared when the active tier is `local`, so the broker's own form
     shows (platform admins only);
   - on each IdP, the mapper `continuity-assurance`
     ([assurance](#assurance)). Without it a session proves nothing beyond
     its IdP's default, so it never holds up sign-ins; `Ready` says why.
4. **Keep the egress path:** a ServiceEntry `sv-egress/continuity-<idp>` for
   each external upstream, bound to `sv-egress/egress-waypoint`, so the
   back-channel (probes, token, JWKS, userinfo) leaves the lab under S&V
   policy. A ServiceEntry of that name owned by another instance is left
   alone (and reported); when `spec.egress` changes or goes away, the old
   ones are removed.
5. **Check trust** every 10 minutes and on every change
   ([trust across IdPs](#trust-across-idps)).

Deleting an IdentityContinuity removes its IdPs and ServiceEntries and
clears the redirector. If Keycloak stays unreachable for 5 minutes, the
controller gives up on Keycloak (event `CleanupAbandoned`) rather than hold
up the deletion.

Users signing in through an upstream are linked to their existing S&V user
by verified email (`idp-detect-existing-broker-user`, `idp-auto-link`), so
`sub` and group membership never change. Each managed IdP carries a username
mapper (`username-from-email`) that makes the brokered username the verified
email, because the first-broker-login lookup matches by email or username:
without it, an upstream account named `bob` would be linked to S&V's Bob
whatever its email.

The controller's RBAC in `sv-identity` is the IdentityContinuity resources, a
Lease, events, the profile-sync CronJob, and `get` on the Secrets it is
configured with, by name
(Role `continuity-controller-secrets`). It cannot list or watch Secrets, so it
never sees Keycloak's own admin or signing-key Secrets. Credentials are re-read
on every reconcile, so a rotated secret takes effect within one interval. The DNS capture that makes the
ServiceEntry apply is Istio ambient's (`AMBIENT_DNS_CAPTURE`, on in 1.31).

## Directory sync

The IdPs in the chain must agree on who each employee is and what their
profile says, so that whichever one is active signs them in with the same
details. The directory sync keeps them in step with the primary, on a
schedule (`spec.sync`), through S&V's profile on the broker:

1. **Primary → S&V's profile.** The chain's first IdP is the primary, and
   says who exists. The sync first gives the broker an account for each of
   its users whose verified email is under one of `spec.profile.domains`
   (`status.sync.provisioned`). Then, for each employee, it reads their
   record from the primary's directory and writes the mapped attributes, and
   their groups ([Groups](#groups)), into S&V's profile, the standard every
   IdP maps to.
2. **S&V's profile → each failover.** It then writes S&V's profile to every
   other IdP with a directory, through that IdP's mapping. A user the
   primary has and a failover doesn't is created there, and the failover
   emails them to set their own password (Keycloak's "execute actions"
   email; the realm needs its SMTP settings). A directory that can't create
   a user without a password (Auth0) reports them instead. The user's
   groups go out too, where the directory can write them (Keycloak groups,
   Auth0 roles).

The lab's workforce realm has no SMTP server: a user the sync creates there
is listed in the run's message until the realm has one, and can't sign in
at that IdP before then.

Reorder the chain and the roles follow. A user is found at an IdP by the
broker's link to it, else by email (one match, never a guess). After each
write the sync reads the record back, so an attribute the directory drops
is reported. It never reads or writes a password or other credential,
never writes the username, and never deletes a user; accounts with role
`local-only` are left alone. Tokens carry the standard claims only.

```yaml
spec:
  profile:
    attributes: [{name: department, displayName: Department}]
  sync: {schedule: "0 2 * * *"}
  tiers:
  - name: auth0                      # primary: read
    attributes:
    - {attribute: firstName, path: given_name}
    - {attribute: department, path: user_metadata.department}
    directory: {type: auth0, url: https://<tenant>/api/v2, audience: https://<tenant>/api/v2/,
      credentialsRef: {name: directory-auth0}}
  - name: keycloak                   # failover: written
    attributes:
    - {attribute: firstName, path: firstName}
    - {attribute: department, path: department}
    directory: {type: keycloak, url: http://keycloak.sv-workforce.svc/admin/realms/workforce,
      credentialsRef: {name: directory-keycloak}}
  - name: contingency                # failover: written
    attributes:
    - {attribute: department, path: department}
    directory: {type: keycloak, url: http://keycloak.sv-contingency.svc/admin/realms/contingency,
      credentialsRef: {name: directory-contingency}}
```

| Directory | Client | Paths |
| --- | --- | --- |
| `scim` (Gluu, Ping, most enterprise IdPs) | `client_credentials` with the SCIM read and write scopes | `name.givenName`, `emails[primary eq true].value`, `urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department` |
| `auth0` | Machine-to-Machine app for the Management API, `read:users`, `update:users` | `given_name`, `user_metadata.department`, `app_metadata.<key>` |
| `keycloak` | service-account client with `view-users`, `manage-users` in that realm | `firstName`, `department` (the realm's user profile must keep it, or allow unmanaged attributes) |

Auth0 has no SCIM API for its own users; its Management API is the
directory. **Test connection** (`sync --test-tier <idp>`, a Job from the
CronJob) gets a token, counts the users and reads the directory's attribute
schema along the sync's own path and credentials; its result (never user
records) is the container's termination message.

The sync runs as ServiceAccount `continuity-sync`: `get` on its instance,
`patch` on its status, and `get` on its own Secret and the directories'
Secrets by name (Role `continuity-sync-secrets`). It reaches the broker and
the directories under the same mesh policy as the controller; directory
hosts get ServiceEntries on the IdP's egress path.

### Groups

The shape has groups as well as attributes (`spec.profile.groups`; the lab's
from `SV_GROUPS` in `config/lab.env`: `advisors`, `platform-engineers`,
`compliance`). Policies and roles name them (an agent workload acting for an
advisor; the Solo UI's administrators), and membership always comes from an
IdP:

- **At sign-in.** Each IdP says which groups the user is in, in its ID token
  (`tiers[].groups.claim`, `<NAME>_GROUPS_CLAIM` in `.env`, `groups` unless
  set). The controller keeps one of Keycloak's own *Advanced Claim to Group*
  mappers per shape group on that IdP, in sync mode *Force*: every sign-in
  sets the user's groups from the claim, so a group the IdP takes away is
  gone at the next sign-in. A namespaced claim (Auth0's) is read whole.
- **In the directory sync.** The primary's groups (or roles) are read into
  the broker, by name, and written out to each failover, so that the next
  IdP in the chain asserts the same groups after a failover.

The broker's own groups beyond the shape are left alone: `platform-admins`,
its break-glass accounts, never comes from an IdP. The sync creates the
shape's groups at the broker (it holds `manage-users`); the controller only
manages the mappers.

### Keeping a lab's profile and mappings

The install maps each IdP's name and email to S&V's profile (Auth0:
`given_name`, `family_name`, `email`). Anything beyond that, such as a
`department` attribute and where each IdP keeps it, is set in the
Observatory's Directory sync window, and lives in the cluster. To keep it
across `make down` / `make up`, put it in `config/continuity.local.yaml`
(gitignored; `config/continuity.example.yaml` shows the format): profile
attributes, each IdP's mappings, an optional sync schedule. A fresh install
takes it as it is; `make layer-47` on an existing lab adds only what the
instance doesn't have yet.

## Kill switch

A real network partition. The controller never reads a flag; it only sees
its probes fail.

```yaml
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: continuity-partition-<idp>
  namespace: sv-egress                       # external upstream
  labels: {continuity.lab.solo.io/tier: <idp>}
spec:
  targetRefs: [{group: networking.istio.io, kind: ServiceEntry, name: continuity-<idp>}]
  action: DENY
  rules: [{}]
```

For an upstream inside the lab, the policy goes in that upstream's namespace
without `targetRefs`. Delete the policy to heal. Status marks the IdP
`partitioned` for information only.

With the default health settings (5 s interval, 2 failures, 3 successes),
failover takes about 10 s after the cut and failback about 15 s after the heal.

## Assurance

How strongly someone signed in is decided at their IdP, and the broker keeps
the IdP's word for it. On every sign-in through an upstream, the controller's
mapper on that IdP (`continuity-assurance`) copies the upstream's `acr`, `amr`
(its values joined with spaces) and `auth_time` onto that sign-in's broker
session, and the broker's client scope `continuity-assurance` puts them in
its tokens:

| Claim | From the upstream's | Example |
| --- | --- | --- |
| `idp` | (the IdP that authenticated the session) | `keycloak` |
| `idp_acr` | `acr` | `aal2` |
| `idp_amr` | `amr` | `pwd otp` |
| `idp_auth_time` | `auth_time` | `1791401688` |

They are session notes, not user attributes: what one sign-in proved never
carries over to another session, or to a refresh after the user signed in
somewhere weaker. A claim the IdP didn't send is absent, and whatever relies
on it treats it as unproven.

Each IdP declares what its values mean (`tiers[].assurance`), as NIST SP
800-63B authenticator assurance levels. A session is the highest level any of
its values maps to, else the IdP's `default`. The install sets these, and
keeps what operators change:

| IdP | Maps | Default |
| --- | --- | --- |
| `keycloak` (S&V's own) | `acr aal1` → AAL1, `acr aal2` → AAL2 | AAL1 |
| `contingency` | `acr aal1` → AAL1 | AAL1 |
| `auth0` | `amr mfa` → AAL2, `acr …/pape/policies/2007/06/multi-factor` → AAL2 | AAL1 |
| `okta` | `acr urn:okta:loa:2fa:any` → AAL2, `phr` → AAL2 phishing-resistant, `phrh` → AAL3 phishing-resistant | AAL1 |
| `gluu` | `acr fido2` → AAL2 phishing-resistant | AAL1 |
| `break-glass` | | AAL1 |

An IdP whose policy for S&V's client always takes a second factor says so
with `default: AAL2` (in `config/continuity.local.yaml`, or the Observatory).

S&V's own Keycloak signs the broker's users in at `aal2`: a password, then a
one-time code from an authenticator app (TOTP), by level of authentication
(the realm's `acr.loa.map`; `minimum.acr.value` on S&V's client there). Its
contingency IdP asks for a password only (`aal1`). An app that needs more can
ask for it with `acr_values`: the broker passes them on to the active IdP,
which steps the user up itself. The broker has no authenticators of its own
for the workforce, and never raises a session's level.

## Trust across IdPs

Failing over to an IdP only helps if it still accepts S&V's client the way
the broker uses it: an IdP admin removing the callback, or a change to its
client authentication, shows up at the worst moment otherwise. The
controller checks each upstream in the chain every 10 minutes, on every spec
change, and when the instance's `continuity.lab.solo.io/check-trust`
annotation changes (the Observatory's **Check now**):

| Check | Passes when |
| --- | --- |
| `Callback` | the IdP accepts S&V's client with the broker's callback: an authorization request with `prompt=none` and PKCE, redirects not followed, is answered at `status.tiers[].redirectURI`. It creates no session or user; the IdP may log it as a failed silent sign-in. |
| `ClientAuth` | the IdP takes the broker's client authentication (and, for `private_key_jwt`, PS256) |
| `PKCE` | the IdP takes PKCE S256 |
| `Scopes` | the IdP offers every scope the broker asks for |
| `Claims` | the IdP issues `sub` and `email` (the broker also requires `email_verified` at sign-in) |
| `Assurance` | the acr values the tier maps are ones the IdP asserts, or the IdP issues the `amr` it maps |

Each is `Pass`, `Fail` or `Unknown`: an IdP that doesn't publish what a check
needs (many omit some discovery fields) is unknown, not failing. A failure
sets `TrustConsistent` False, naming the IdP and the check, with a Warning
event `TrustMismatch`. The checks are informational: which IdP signs people
in only ever follows the health probes.

What the workloads see doesn't change across IdPs by construction: they
trust only the broker, so the issuer, the audiences and their own client
registrations at the broker are the same whichever IdP is active. A workload
profile's status lists those registrations.

## Assurance rules

Assurance rules say what a sign-in must prove to reach a resource, whichever
IdP it came through, and which IdPs may vouch for its users:

- **The default rule** (`spec.assurancePolicy` on the IdentityContinuity):
  what every rule starts from, and what a gateway policy that asks the gate
  without naming a rule gets. As installed: AAL1, from any IdP in the chain,
  no break-glass, sessions from any allowed IdP.
- **A rule** (a `WorkloadProfile`, `wlp`, beside its IdentityContinuity in
  `sv-identity`): one group of workloads, and only what differs from the
  default rule.

```yaml
apiVersion: continuity.lab.solo.io/v1alpha1
kind: WorkloadProfile
metadata: {name: advisor-workspace, namespace: sv-identity}
spec:
  continuity: sterling-vance
  description: Bob's workspace tools behind the firm's front door, used by his agents on his behalf
  criticality: Critical
  mode: Enforce
  workloads: [{namespace: sv-mcp, serviceAccount: bob-workspace}]
  clients: [kagent]
  assurance: {minimum: AAL2}            # the rest is the default rule's
```

| Requirement | Default rule (`assurancePolicy`) | A rule (`WorkloadProfile`), unset: the default's |
| --- | --- | --- |
| Minimum assurance | `minimum` (`AAL1` default, `AAL2`, `AAL3`) | `assurance.minimum` |
| Phishing-resistant authenticator | `phishingResistant` | `assurance.phishingResistant` |
| Signed in within (`idp_auth_time`), e.g. `12h` | `maxAge` | `assurance.maxAge` |
| IdPs that may vouch (empty: every upstream) | `allowedIdPs` | `allowedIdPs` |
| Break-glass accounts | `allowBreakGlass` | `allowBreakGlass` |
| Sessions: `Any`, or `ActiveIdPOnly` (only from the IdP signing people in now; one from an IdP the chain has moved off is refused until the user signs in again) | `sessions` | `sessions` |

A rule also has a `mode`, what the gate does with it:

| Mode | A request the rule refuses |
| --- | --- |
| `Enforce` (default) | is refused |
| `ReportOnly` | goes through; the gate logs and returns `would-deny` with its reason, to see a rule's effect before enforcing it |
| `Off` | goes through; the gate doesn't evaluate it |

And its `criticality` (`Critical`, `High`, `Standard`), its `workloads` (mesh
identities), the broker `clients` users sign in through, and optionally
`obligations`, `owner` and `description`, for reporting.

Status, from the controller (and only the controller: a
ValidatingAdmissionPolicy refuses anyone else's status write):

| Field | Meaning |
| --- | --- |
| `phase` | `Available`: sign-ins through the IdP serving now can meet the rule. `Degraded`: they can, on a later IdP than the first that can. `FailedClosed`: they can't, and requests are refused (in `Enforce` mode) |
| `serving`, `servingLevel` | the active IdP, and the most a sign-in through it proves |
| `eligibleIdPs` | the chain's IdPs, in order, whose sign-ins can meet the rule |
| `clients[]` | each client as the broker has it: found, redirect URIs, audiences, and whether its tokens carry the upstream's assurance (`continuity-assurance`) |
| conditions | `Ready` (False: the IdentityContinuity or a client is missing, `allowedIdPs` names an IdP not in the chain, or a client's tokens can't carry assurance while the rule requires more than AAL1) |

Events: `Available`, `Degraded`, `FailedClosed` (Warning, in `Enforce` mode)
as the chain moves.

Bob's demo has three (`demos/bob/manifests/05-workload-profiles.yaml`):

| Rule | Criticality | Requirements | Enforced at |
| --- | --- | --- | --- |
| `advisor-workspace` | Critical | AAL2 | `agentgateway-system/bob-workspace-caller` (ai-gateway, in front of Bob's workspace) |
| `ledgerline-research` | High | AAL2, active IdP only | `agentgateway-system/xaa-ledgerline-caller` (S&V's egress to Ledgerline, before any ID-JAG) |
| `agent-console` | Standard | break-glass allowed | the kagent console's sign-in is at the edge, which doesn't ask the gate |

With S&V's own Keycloak cut, the chain fails over to the contingency IdP:
`advisor-workspace` and `ledgerline-research` go `FailedClosed`, and
`agent-console` keeps serving (`Degraded`).

### The assurance gate

The gate enforces the rules: an Envoy external authorization service
(gRPC), the continuity image's `gate` command, three replicas in
`sv-identity` (`platform/47-continuity/assurance-gate.yaml`). A gateway
policy asks it in the same agentgateway policy as its JWT check:

```yaml
traffic:
  jwtAuthentication: {mode: Strict, providers: [...]}   # the broker's tokens
  extAuth:
    backendRef: {kind: Service, name: assurance-gate, namespace: sv-identity, port: 9001}
    failureMode: FailClosed
    grpc:
      contextExtensions: {profile: advisor-workspace}   # none: the default rule
      requestMetadata:
        continuity: >-
          {"iss": jwt.iss, "sub": jwt.sub, "idp": has(jwt.idp) ? jwt.idp : "",
           "acr": has(jwt.idp_acr) ? jwt.idp_acr : "", "amr": has(jwt.idp_amr) ? jwt.idp_amr : "",
           "auth_time": has(jwt.idp_auth_time) ? jwt.idp_auth_time : 0}
```

`contextExtensions` names the rule (`profile`) and, where the namespace has
more than one chain, the chain (`continuity`). The gate publishes this
stanza itself (`GET /v1/policy-point?rule=&continuity=` on its evaluate
port), so whatever writes a policy point writes what the gate reads.

It reads the verified token's claims as metadata, never a header the caller
could set, and admits the session if the rule takes sessions from that IdP
(`allowedIdPs`, break-glass, `sessions`) and what the IdP asserted meets its
requirements:

| Answer | When |
| --- | --- |
| allowed | the session meets the rule, or the rule is `ReportOnly` (`would-deny` when it wouldn't) or `Off` |
| `401`, `WWW-Authenticate: Bearer error="insufficient_user_authentication"` (RFC 9470) | the user could pass by authenticating more strongly at their IdP: below the minimum, not phishing-resistant, or too long ago. `acr_values` names what to ask the active IdP for, when one of its mapped values meets the rule; `max_age`, when age was the reason |
| `403` | the rule doesn't take this session at all: an IdP not allowed, break-glass, a session from an IdP the chain has moved off (`ActiveIdPOnly`), no verified token, or a rule the gate doesn't know |
| `503` | the gate can't decide: it hasn't read the rules yet, or, for an `ActiveIdPOnly` rule, it has had no word from the chain for 30 s |

Each answer carries `x-continuity-decision: <allow|deny|would-deny|off|unavailable>
<rule>: <reason>`, which S&V's gateways log (`continuity.decision`), and a
refusal's body names the rule and the reason.

**What the rules decide, asked of the gate.** On a second port (`evaluate`,
9002), the gate answers from the same code that enforces the rules:
`POST /v1/evaluate` with a chain, optionally a draft of its rules (the
default rule, IdPs' assurance, rules added, changed or removed), and
optionally one session (`idp`, `acr`, `amr`, `authTime`). For every rule
and the default rule it returns the phase, the IdPs that can meet it, each
IdP's outcome (`Admit`; `Conditional`, when the IdP asserts the value named;
`Refuse`) with the reason, and the session's decision. It is read only, and
only the Observatory may call it.

It is built not to become what it protects against:

- no secrets, and no call to the broker or an IdP per request;
- it watches the rules and the chain, so a failover reaches it as it
  happens, and decides from what it last saw, so an API server outage
  changes nothing but `ActiveIdPOnly` rules (the controller's status writes
  are its heartbeat);
- three replicas spread across zones and nodes, a PodDisruptionBudget of 2;
- only the gateways whose policies ask it may call its gRPC port
  (AuthorizationPolicy `assurance-gate-callers`, and one per gateway the
  Observatory turns enforcement on at), and a ValidatingAdmissionPolicy
  (`assurance-gate-fail-closed`) refuses any policy that asks it without
  failing closed: with no gate, requests are refused, never let through.

The gate's Service carries the label `continuity.lab.solo.io/assurance-gate`,
with ports named `grpc` and `evaluate`: that, not its name, is how the
Observatory finds it, and how the admission policy knows which policies must
fail closed (each labelled Service is one of its parameters).

Changing the default rule or a rule changes the gate's next decision; no
gateway policy is touched. A rule no gateway policy asks for isn't enforced,
whatever it says: the Observatory says so.

## In the Observatory

**Identity Continuity** tab, one `IdentityContinuity` at a time:

![Connected: Auth0 signing people in, through the S&V egress](images/observatory-continuity-connected.jpg)

![Failover: the network to Auth0 cut at the egress, sign-in through the next IdP](images/observatory-continuity-failover.jpg)

- **Map:** every app that signs people in through the broker (found from the
  edge's SSO configuration), the broker, the egress gateway, and the IdPs in
  chain order. The live path is green.
- **Banner:**

| State | Banner |
| --- | --- |
| first IdP active | green: `CONNECTED · <idp> SIGNING PEOPLE IN` |
| cut, not failed over yet | amber, pulsing: `OUTAGE · <idp> UNREACHABLE · FAILING OVER`, with failed checks counted |
| failed over | red, pulsing: `FAILOVER ACTIVE · <active> → REPLACING <first>`, with who cut what and when |
| healed, verifying | amber: `<idp> ANSWERING AGAIN · VERIFYING BEFORE FAILING BACK`, with healthy checks counted |
| healthy again, failback Manual | amber: `<active> SIGNING PEOPLE IN · <first> HEALTHY, FAILBACK IS MANUAL` |
| nothing can take sign-ins (break-glass disabled too) | red: `SIGN-IN UNAVAILABLE · NO HEALTHY IDP` |

- **Simulate IdP outage / Restore IdP network:** creates or deletes the kill
  switch policy for the active IdP (a picker appears with more than one).
  The cut wire reads `NETWORK CUT`, the IdP is stamped `OUTAGE`, and the
  egress gateway turns red. The policy records who cut it.
- **Rule builder** (right): IdP order, enable, drain, failover conditions,
  latency limit, client secrets (write-only), new OIDC IdPs (the redirect URI
  to register is shown), health settings, and **Fail back automatically**. Save applies the spec as
  you.
- **Directory sync** (from the rule builder): the IdPs in chain order on
  the left (primary, failovers), S&V's profile on the right. Each IdP lists
  the attributes its directory has, as the sync or **Test connection** last
  detected them (Auth0: its profile fields and the metadata keys its users
  carry); wire them to the S&V attributes they pair with. Attributes are
  added only to S&V's profile (**+ attribute**: name, display name, type,
  list). Each IdP's **Directory · Edit** sets its type (`scim`, `auth0`,
  `keycloak`), URL, scopes, audience (Auth0) and credentials (write-only,
  written to Secret `directory-<idp>`); **Remove** stops syncing it. Each
  IdP's directory has **Test connection** (the saved settings, run as the
  sync). **Code** edits the same mapping as JSON, each S&V
  attribute and the IdPs' attributes paired with it, in chain order
  (`"department": ["auth0.user_metadata.department", "keycloak.department"]`);
  **Schedule** sets the sync's cron, pauses it, shows the last run and runs
  it now. Directory credentials are write-only and readable by the sync
  alone.

  ![Directory sync canvas: Auth0's detected attributes wired to S&V's profile, S&V's own Keycloak written from it](images/observatory-directory-sync-canvas.jpg)

  ![The same mapping as JSON](images/observatory-directory-sync-code.jpg)

  ![Schedule: cron presets, next runs, the last run and Run now](images/observatory-directory-sync-schedule.jpg)

- **Transitions** and **Identity traffic** (bottom): failovers, cuts and
  restores, and OIDC calls.

**Assurance rules** (a button in the rule builder, beside Directory sync):
the rules, what relies on the broker without one, and the default rule, each
in the same form; the IdPs and what their sign-ins prove; a what-if for one
sign-in; and the same as HCL
([OBSERVATORY.md](OBSERVATORY.md#identity-continuity)). What each rule
decides comes from the assurance gate. Each IdP in the rule builder shows its
trust checks as a badge, and the banner names the enforced rules failing
closed.

## IdP setup

Each IdP, the settings it takes and how to register S&V there:
[IDPS.md](IDPS.md) (S&V's own Keycloak and contingency IdP, Auth0, Gluu,
Okta, Ping, any other OIDC provider).

## Checks

`make continuity-verify` runs on S&V's own Keycloak with the IdPs ahead of
it drained, then restores the spec: controller health and leadership; Bob's
browser sign-in through it (password and one-time code); a partition in
`sv-workforce` failing over to the contingency IdP, Bob signing in there,
then that cut too, failing over to `break-glass` (the broker's form), and
back; Manual and Automatic failback; the directory sync written out to a
failover and read in from the primary, with `status.sync` and Test
connection; an IdP removed and re-added at runtime; the trust checks, and the
broker's callback removed at S&V's own Keycloak and caught; and, with an
external IdP configured, its partition at `sv-egress` and the browser landing
on it. `keycloak` must be in `ENTERPRISE_IDP`. Bob's sign-in at an external
IdP is interactive and is skipped.

`make bob-verify` checks assurance rules end to end: Bob's AAL2 session
admitted to his workspace and to Ledgerline; S&V's own Keycloak cut, the
rules `FailedClosed` on the contingency IdP and his new AAL1 session refused
at both (401 with the challenge); his session from before the outage still
working where any allowed IdP's sessions count, and refused where only the
active IdP's do; a rule change taking effect with no gateway change;
`allowedIdPs`; `ReportOnly` letting the refused session through as
`would-deny`; and the gate losing a replica, then all of them (refused,
never let through).

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| IdP `NotConfigured` | `<NAME>_CLIENT_ID` missing from `.env` (and `<NAME>_CLIENT_SECRET`, for an IdP that uses one), or `make layer-47` not re-run |
| Auth0: callback URL mismatch | the callback isn't exactly `status.tiers[].redirectURI` |
| Keycloak: "does not match the configured essential claim" | the user's email isn't verified at the upstream (`email_verified: false`) |
| Keycloak: `invalid_client` | `private_key_jwt`: the IdP doesn't have `sv-upstream-client.jwks.json` from `make xaa-keys`; a client secret: the IdP's method isn't Client Secret (Post), or the secret is stale |
| outage button: "no network path is known" | no ServiceEntry for the IdP (`spec.egress` unset) and no namespace labelled `continuity.lab.solo.io/tier: <idp>`, or the active tier is `break-glass` (`local`) |
| cut has no effect | the caller isn't in the ambient mesh, or DNS capture is off, so traffic bypasses the ServiceEntry |
| stays on `break-glass` after a heal | failback waits for `healthyThreshold` successes; `Manual` failback never moves back |
| `ProfileApplied` False: maps to "x", not in the profile | add the attribute to S&V's profile, or fix the mapping |
| sync: `directory token: HTTP 401` | the directory client's credentials, or it lacks `client_credentials` |
| sync: `directory: HTTP 403` | the directory client lacks the read scope (`scopes`) or Auth0 `read:users` |
| sync failed, attribute unchanged | that IdP's directory was unreachable; its attributes are kept until the next run |
| `TrustConsistent` False: `<idp> Callback` | the IdP doesn't accept S&V's client with `status.tiers[].redirectURI`: register the callback there again |
| `TrustConsistent` False: `<idp> Assurance` | none of the acr values the tier maps are ones the IdP asserts: fix `tiers[].assurance` |
| `401 insufficient_user_authentication` from a workload | the session's IdP didn't assert enough for its profile (the body says what it had): sign in again with a second factor there, or, failed over to a weaker IdP, wait for failback |
| `403` naming a resource's rules | they don't take sessions from that IdP (`allowedIdPs`, break-glass, or `ActiveIdPOnly` after a failover: sign in again) |
| `503` naming a resource's rules | the gate hasn't read them yet, or rules with `ActiveIdPOnly` haven't heard from the chain in 30 s (the continuity controller, or the API server) |
| profile `Ready` False: `ClientWithoutAssurance` | the client's tokens don't carry `idp_acr`: add client scope `continuity-assurance` to it at the broker |
| every session counts as AAL1 | the IdP sends neither an acr nor an amr the tier maps; or its mapper `continuity-assurance` is missing (`Ready` says so): the broker's image lacks the session-claims provider |

Keycloak's log names the claim or step that failed:

```bash
kubectl --context kind-solo-lab -n sv-identity logs deploy/keycloak --since=10m | grep -i -E "claim|IDENTITY_PROVIDER"
```
