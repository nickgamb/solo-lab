# Moving to Solo Enterprise

Every product switches independently. Set the edition in `.env` and re-run
`make platform`:

```bash
AGW_EDITION=enterprise          # one product
EDITION=enterprise              # all of them
SOLO_LICENSE_KEY=...            # or SOLO_AGW_LICENSE_KEY, SOLO_KGATEWAY_LICENSE_KEY, ...
```

`config/enterprise.env` holds the enterprise charts, versions and kinds.
`scripts/lib.sh` promotes them over the OSS pins for each product set to
`enterprise`. Trial keys are often product-scoped (kagent-enterprise rejects
an agentgateway key), so set the per-product key when you have one.

| Product | OSS here | Enterprise | What changes in the lab |
| --- | --- | --- | --- |
| Istio | upstream 1.31.1 (tarball charts) | Solo Enterprise for Istio `1.31.1-solo` | Solo-built images via `global.hub`/`tag`, license on istiod, peering enabled. Nothing else. |
| kgateway | 2.4.5, GatewayClass `kgateway` | Solo Enterprise for kgateway 2.3.5, `enterprise-kgateway` | GatewayClass name (templated). The edge SSO can move from the OAuth2 filter to Enterprise ext-auth. |
| agentgateway | 1.5.0, `agentgateway` | Solo Enterprise for agentgateway v2026.9.2, `enterprise-agentgateway` | Parameters kind `EnterpriseAgentgatewayParameters` (templated). Enterprise adds an STS/OBO service, which can replace the RFC 8693 hop to Keycloak. |
| kagent | 0.10.2 + `tools/kagent` patches | kagent-enterprise (tracks: `ga` 0.5.8 + management plane, or `1.0` alpha on Substrate 0.2.x) | Needs an OIDC issuer (S&V Keycloak). Its `rbac.roleMapping` reads `claims.Groups`, so add a `Groups` mapper or override the CEL. |
| agentregistry | 0.4.0 behind edge SSO | agentregistry-enterprise 2026.9.0 | Native OIDC: clients `are-backend` and `are-cli` with a `groups` mapper. It runs as root, so there's no `restricted` PSA on its namespace. |
| Management UI | — | `management` 0.5.8 (`products.{mesh,agentgateway,kagent}`) | OIDC clients `kagent-backend` and `kagent-ui`. |

The patches under `tools/` are independent of edition. Drop each one when
its upstream change ships.
