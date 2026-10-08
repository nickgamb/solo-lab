# Keycloak with ID-JAG issuing (PR #49998, backported to 26.7.4)

Keycloak can *receive* ID-JAGs today (`identity-assertion-jwt` feature) but not
*issue* them. [keycloak/keycloak#49998](https://github.com/keycloak/keycloak/pull/49998)
(issue [#48818](https://github.com/keycloak/keycloak/issues/48818)) adds issuing
through the token endpoint. This directory builds stock Keycloak 26.7.4 with
that PR applied, so Sterling & Vance's IdP can act as the enterprise IdP in
Cross App Access.

- `patches/0001-backport-pr49998-idjag-issuer-26.7.4.patch`: the PR's single
  commit (866d795), rebased onto the 26.7.4 tag. Two conflicts, both mechanical:
  - `META-INF/services/...TokenExchangeProviderFactory`: main also lists
    `TokenExchangeDelegationProviderFactory`; 26.7.4 does not.
  - `StandardTokenExchangeProvider`: the PR extracts the response build into
    `buildTokenExchangeResponse(...)`. main's version of that block also fires
    a `TokenExchangeResponseContext` client-policy event, a class that doesn't
    exist in 26.7.4, so the backport keeps 26.7.4's behavior and drops it.
- Only `keycloak-core` and `keycloak-services` change. The Dockerfile rebuilds
  those two jars and overlays them on `quay.io/keycloak/keycloak:26.7.4`.

## Contract (from the PR)

```
POST /realms/<realm>/protocol/openid-connect/token     (client authenticated)
  grant_type=urn:ietf:params:oauth:grant-type:token-exchange
  requested_token_type=urn:ietf:params:oauth:token-type:id-jag
  subject_token=<ID token issued to the requesting client>   # ID token only
  subject_token_type=urn:ietf:params:oauth:token-type:id_token
  audience=<clientId of the client representing the resource AS>
  scope=<optional scopes of the requesting client>
-> { access_token: <ID-JAG, typ oauth-id-jag+jwt>, issued_token_type: ...:id-jag, token_type: N_A }
```

Configuration, as in the PR's `IDJAGTokenExchangeTest`:

- The requesting client has `standard.token.exchange.enabled=true`.
- The resource authorization server is a client whose `clientId` is its issuer
  URL. That becomes the ID-JAG's `aud`.
- A client scope on the requesting client carries a Hardcoded Claim mapper,
  `client_id=<the requester's client id at the resource AS>`. This is the
  admin-approved app-to-app connection.
- The subject ID token must have a live user session and client session.
  Logout or revocation stops new ID-JAGs.

## Identity continuity's IdP mapper (`session-claims/`)

S&V's broker keeps how the upstream IdP authenticated each sign-in (its `acr`,
`amr` and `auth_time`) on that sign-in's session, so workload profiles can
require an assurance level (docs/IDENTITY-CONTINUITY.md). Keycloak's own
claim-to-session-note mapper copies string claims only; `amr` is an array and
`auth_time` a number. `session-claims/` is a small provider,
`continuity-session-claims-idp-mapper`, that copies all three as strings (an
array's values joined with spaces) to session notes `continuity.<claim>`. A
claim the upstream didn't send leaves no note. The continuity controller puts
it on every upstream IdP (`continuity-assurance`, sync mode FORCE, so every
sign-in), and the broker's `continuity-assurance` client scope puts the notes
in tokens as `idp_acr`, `idp_amr` and `idp_auth_time`.

The image compiles it against the Keycloak jars it builds and loads it from
`/opt/keycloak/providers`. It is independent of the ID-JAG patch and stays
when that patch goes.

## Moving to upstream

When #49998 merges and ships in a release, drop `patches/` and the two jars
the Dockerfile replaces, and build the provider onto the stock release image.
The realm configuration stays the same.
