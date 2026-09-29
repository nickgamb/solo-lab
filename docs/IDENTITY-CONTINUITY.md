# Identity continuity

Sterling & Vance's Keycloak (`https://idp.sterling.lab`, realm `sterling-vance`)
brokers workforce sign-in to an ordered chain of upstream IdPs and falls back
to its own accounts. Keycloak stays the only issuer anything in the lab
trusts; an upstream only authenticates the person. When the upstream goes
down, new sign-ins move to the next healthy tier and nothing downstream
changes: same issuer, same `sub`, same groups.

![Identity continuity at 4x speed: the upstream IdP signing people in, a simulated outage at the firm's egress, failover to S&V's own accounts, and failback](videos/identity-continuity.gif)

- API and controller: `apps/continuity` (`IdentityContinuity`, `continuity.lab.solo.io/v1alpha1`)
- Install: `platform/47-continuity` (`make layer-47`, after `45-identity`)
- Demo card: [cards/identity-continuity.html](cards/identity-continuity.html)
- Checks: `make continuity-verify`

```bash
kubectl --context kind-solo-lab get idc -n sv-identity
```

## The resource

The installed rule is `platform/47-continuity/identitycontinuity.yaml`: Auth0
first, S&V's local accounts second, automatic failback. Without `AUTH0_ISSUER`
it is installed with the local tier only. It is applied once; after that the
spec belongs to its operators (the Observatory rule builder edits it), and
re-running the layer changes only the auth0 tier's issuer, when
`AUTH0_ISSUER` has changed.

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
  config is realm config), with its credentials in the Secret
  `broker.keycloak.credentialsRef` names.

### spec

| Field | Meaning |
| --- | --- |
| `broker.keycloak.url` | in-cluster Keycloak base URL (admin API) |
| `broker.keycloak.realm` | realm to manage |
| `broker.keycloak.credentialsRef` | Secret with `client-id`/`client-secret` of a service-account client with realm-management `manage-identity-providers` and `manage-realm` |
| `broker.keycloak.browserFlow` | browser flow whose Identity Provider Redirector points at the active tier (`continuity-browser`) |
| `broker.keycloak.firstBrokerLoginFlow` | first-broker-login flow for every upstream (`continuity-first-broker-login`: link to the existing user) |
| `egress.namespace`, `egress.waypoint` | route back-channel calls to external upstreams through this waypoint (one ServiceEntry per tier) |
| `egress.internalDomains` | hosts under these domains are in-cluster and get no ServiceEntry |
| `tiers[]` | ordered; the first eligible, healthy tier is active |
| `tiers[].name` | also the Keycloak IdP alias |
| `tiers[].displayName` | shown on the login page and in the Observatory |
| `tiers[].type` | `oidc` (an upstream) or `local` (the broker's own accounts) |
| `tiers[].enabled` | `false`: never active, Keycloak IdP disabled |
| `tiers[].drain` | out of rotation; sessions in flight keep working |
| `tiers[].oidc.issuer` | exactly as the upstream publishes it (Auth0's ends in `/`) |
| `tiers[].oidc.clientID` | optional; falls back to the Secret's `client-id` key |
| `tiers[].oidc.clientSecretRef` | Secret and key (default `client-secret`); missing means `NotConfigured` |
| `tiers[].oidc.scopes` | default `openid email profile` |
| `tiers[].failoverWhen` | which probe results count against the tier: `unreachable`, `serverError`, `invalidDiscovery` (each default true), `latencyAboveMs` (must be below `health.timeoutSeconds`: a slower answer times out first) |
| `health` | `intervalSeconds`, `timeoutSeconds`, `unhealthyThreshold` (failures in a row to go down), `healthyThreshold` (successes in a row to come back) |
| `failback` | `Automatic` (move back up as soon as a higher tier is healthy) or `Manual` |

### status

`active` (where logins go now: the tier Keycloak's redirector points at, or
the local tier), `activeSince`, `broker.issuer`, `egressNamespace`, per tier (`configured`, `healthy`,
`partitioned`, `latencyMs`, `reason`, `message`, consecutive failures and
successes, and `redirectURI`: the callback the upstream app must allow), the
last 20 `transitions`, and conditions `Ready` and `Degraded` (not on the first
tier). The controller also emits Kubernetes events (`TierHealthy`,
`TierUnhealthy`, failovers).

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
     login page when not eligible), with PKCE S256, `client_secret_post`, and
     a claim filter that only accepts `email_verified: true`. A setting
     changed by hand in Keycloak is put back. If a tier's Secret goes
     missing its IdP stays (hidden), so users' links to it survive; only
     removing the tier from the spec deletes it;
   - the `continuity-browser` flow's IdP redirector set to the active tier,
     or cleared when the active tier is `local`, so S&V's own form shows.
4. **Keep the egress path:** a ServiceEntry `sv-egress/continuity-<tier>` for
   each external upstream, bound to `sv-egress/egress-waypoint`, so the
   back-channel (probes, token, JWKS, userinfo) leaves the lab under S&V
   policy. A ServiceEntry of that name owned by another instance is left
   alone (and reported); when `spec.egress` changes or goes away, the old
   ones are removed.

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
Lease, events, and `get` on the Secrets it is configured with, by name
(Role `continuity-controller-secrets`). It cannot list or watch Secrets, so it
never sees Keycloak's own admin or signing-key Secrets. Credentials are re-read
on every reconcile, so a rotated secret takes effect within one interval. The DNS capture that makes the
ServiceEntry apply is Istio ambient's (`AMBIENT_DNS_CAPTURE`, on in 1.31).

## Kill switch

A real network partition. The controller never reads a flag; it only sees
its probes fail.

```yaml
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: continuity-partition-<tier>
  namespace: sv-egress                       # external upstream
  labels: {continuity.lab.solo.io/tier: <tier>}
spec:
  targetRefs: [{group: networking.istio.io, kind: ServiceEntry, name: continuity-<tier>}]
  action: DENY
  rules: [{}]
```

For an upstream inside the lab, the policy goes in that upstream's namespace
without `targetRefs`. Delete the policy to heal. Status marks the tier
`partitioned` for information only.

With the default health settings (5 s interval, 2 failures, 3 successes),
failover takes about 10 s after the cut and failback about 15 s after the heal.

## In the Observatory

**Identity Continuity** tab, one `IdentityContinuity` at a time:

![Connected: Auth0 signing people in, through the S&V egress](images/observatory-continuity-connected.jpg)

![Failover: the network to Auth0 cut at the egress, sign-in on S&V's own accounts](images/observatory-continuity-failover.jpg)

- **Map:** every app that signs people in through the broker (found from the
  edge's SSO configuration), the broker, the egress gateway, and the tiers in
  order. The live path is green.
- **Banner:**

| State | Banner |
| --- | --- |
| first tier active | green: `CONNECTED · <tier> SIGNING PEOPLE IN` |
| cut, not failed over yet | amber, pulsing: `OUTAGE · <tier> UNREACHABLE · FAILING OVER`, with failed checks counted |
| failed over | red, pulsing: `FAILOVER ACTIVE · <active> → REPLACING <first>`, with who cut what and when |
| healed, verifying | amber: `NETWORK RESTORED · VERIFYING <tier> BEFORE FAILING BACK`, with healthy checks counted |
| no healthy tier | red: `SIGN-IN UNAVAILABLE · NO HEALTHY TIER` |

- **Simulate IdP outage / Restore IdP network:** creates or deletes the kill
  switch policy for the active upstream (a picker appears with more than one
  upstream). The cut wire reads `NETWORK CUT`, the tier is stamped `OUTAGE`,
  and the egress gateway turns red. The policy records who cut it.
- **Rule builder** (right): tier order, enable, drain, failover conditions,
  latency limit, client secrets (write-only), new OIDC tiers (the redirect URI
  to register is shown), health settings, failback. Save applies the spec as
  you.
- **Transitions** and **Identity traffic** (bottom): failovers, cuts and
  restores, and OIDC calls.

## Auth0 setup

Tenant: `AUTH0_ISSUER` in `.env`, your tenant's issuer with its trailing
slash (`https://<tenant>.us.auth0.com/`). Unset, the lab has no auth0 tier.

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

3. **Create the user** `bob@sterling.lab` with a password of your choice, and
   **mark the email verified** (edit the email on the user's page, or
   `PATCH /api/v2/users/{id}` with `{"email_verified": true}` from the
   Management API Explorer). Unverified emails are refused at Keycloak.

4. **Give the lab the tenant and credentials** in `.env`, then install the
   layer again:

   ```
   AUTH0_ISSUER=https://<tenant>.us.auth0.com/
   AUTH0_CLIENT_ID=<Client ID>
   AUTH0_CLIENT_SECRET=<Client Secret>
   ```

   ```bash
   make layer-47
   ```

   This writes Secret `sv-identity/upstream-auth0`. Within three probes the
   tier is healthy and, with automatic failback, active:

   ```bash
   kubectl --context kind-solo-lab get idc sterling-vance -n sv-identity -o jsonpath='{.status.active}'
   ```

Bob's groups come from his S&V account, not from Auth0.

## Another upstream IdP

Any OIDC provider works (Okta, Entra ID, Ping, a second Keycloak). In the
rule builder, **Add OIDC tier**: name, display name, issuer, client ID and
secret. Register `<broker issuer>/broker/<name>/endpoint` as the app's
callback (shown in the form and in `status.tiers[].redirectURI`), allow
`openid email profile`, use `client_secret_post`, and make sure the IdP
sends `email_verified: true` for the user. The rule builder also grants the
controller read access to the new tier's Secret. Editing `spec.tiers` by hand,
create the Secret and add its name to Role `sv-identity/continuity-controller-secrets`
(the controller reads Secrets by name only, and never lists or watches them).

## Checks

`make continuity-verify` runs the loop on a scratch tier pointed at an issuer
the lab already runs (Ledgerline's Keycloak), then restores the spec:
controller health and leadership, a tier added at runtime, failover by
partition, failback on heal, live rule changes, cleanup of a removed tier,
and, when Auth0 is configured, the egress partition on `continuity-auth0`
and a browser sign-in landing on Auth0. Bob's Auth0 sign-in itself is
interactive and is skipped.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| tier `NotConfigured` | `AUTH0_CLIENT_ID`/`AUTH0_CLIENT_SECRET` missing from `.env`, or `make layer-47` not re-run |
| Auth0: callback URL mismatch | the callback isn't exactly `status.tiers[].redirectURI` |
| Keycloak: "does not match the configured essential claim" | the user's email isn't verified at the upstream (`email_verified: false`) |
| Keycloak: `invalid_client` | Authentication Method isn't Client Secret (Post), or the secret is stale |
| outage button: "no network path is known" | no ServiceEntry for the tier: `spec.egress` unset, or the tier is `local` |
| cut has no effect | the caller isn't in the ambient mesh, or DNS capture is off, so traffic bypasses the ServiceEntry |
| stays on local after a heal | failback waits for `healthyThreshold` successes; `Manual` failback never moves back |

Keycloak's log names the claim or step that failed:

```bash
kubectl --context kind-solo-lab -n sv-identity logs deploy/keycloak --since=10m | grep -i -E "claim|IDENTITY_PROVIDER"
```
