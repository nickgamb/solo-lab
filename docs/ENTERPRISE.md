# Moving to Solo Enterprise

Trial licences and pricing: [solo.io/get-started](https://www.solo.io/get-started). Product
documentation for every edition: [docs.solo.io](https://docs.solo.io).

Istio, kgateway and agentgateway switch independently. Set the edition in
`.env` and re-run `make platform`:

```bash
AGW_EDITION=enterprise          # one product
ISTIO_EDITION=enterprise KGATEWAY_EDITION=enterprise AGW_EDITION=enterprise
SOLO_LICENSE_KEY=...            # or SOLO_AGW_LICENSE_KEY, SOLO_KGATEWAY_LICENSE_KEY, ...
```

`EDITION=enterprise` flips every product, kagent included, and kagent isn't
wired for enterprise yet, so today use the per-product switches.

`config/enterprise.env` holds the enterprise charts, versions and kinds.
`scripts/lib.sh` promotes them over the OSS pins for each product set to
`enterprise`. Trial keys are often product-scoped (kagent-enterprise rejects
an agentgateway key), so set the per-product key when you have one.

**Status.** "Wired" means the installs switch charts, images and kinds; none
has been run against a licence in this lab yet, so treat the first
enterprise run as a validation pass and expect to adjust.

| Product | OSS here | Enterprise | Status in this lab | What changes |
| --- | --- | --- | --- | --- |
| [Istio](https://docs.solo.io/istio/) | upstream 1.31.1 (tarball charts) | Solo Enterprise for Istio `1.31.1-solo` | wired, not yet validated | Solo-built images via `global.hub`/`tag`, license on istiod, peering enabled. Nothing else. |
| [kgateway](https://docs.solo.io/kgateway/) | 2.4.5, GatewayClass `kgateway` | Solo Enterprise for kgateway 2.3.5, `enterprise-kgateway` | wired, not yet validated | GatewayClass name (templated). The edge SSO can move from the OAuth2 filter to Enterprise ext-auth. |
| [agentgateway](https://docs.solo.io/agentgateway/) | 1.5.0, `agentgateway` | Solo Enterprise for agentgateway v2026.9.2, `enterprise-agentgateway` | wired, not yet validated | Parameters and policy kinds (`EnterpriseAgentgatewayParameters`, `EnterpriseAgentgatewayPolicy`, templated, ReferenceGrants included). Enterprise adds an STS/OBO service, which can replace the RFC 8693 hop to Keycloak. |
| [kagent](https://docs.solo.io/kagent/) | 0.10.2 + `tools/kagent` patches | kagent-enterprise (tracks: `ga` 0.5.8 + management plane, or `1.0` alpha on Substrate 0.2.x) | **not wired**: layer 60 stops with `KAGENT_EDITION=enterprise` | A different chart and values. It needs an OIDC issuer (S&V Keycloak), and its `rbac.roleMapping` reads `claims.Groups`, so add a `Groups` mapper or override the CEL. The `1.0` track also needs Substrate 0.2.x. |
| [agentregistry](https://docs.solo.io/agentregistry/) | 0.4.0 behind edge SSO | agentregistry-enterprise 2026.9.0 | **not wired**: no edition switch | Native OIDC: clients `are-backend` and `are-cli` with a `groups` mapper. It runs as root, so there's no `restricted` PSA on its namespace. |
| Management UI | — | `management` 0.5.8 (`products.{mesh,agentgateway,kagent}`) | not installed | OIDC clients `kagent-backend` and `kagent-ui`. |

The patches under `tools/` are independent of edition. Drop each one when
its upstream change ships.
