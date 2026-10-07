# Moving to Solo Enterprise

Trial licences and pricing: [solo.io/get-started](https://www.solo.io/get-started). Product
documentation for every edition: [docs.solo.io](https://docs.solo.io).

Every product runs OSS by default and switches to Solo Enterprise on its own.
Set the edition in `.env`, then `make platform` (or `make up` for a new
cluster):

```bash
EDITION=enterprise                # every product
AGW_EDITION=enterprise            # or one: ISTIO_, KGATEWAY_, AGW_, KAGENT_, AGENTREGISTRY_EDITION
SOLO_LICENSE_KEY=...              # one key for all, or per product:
SOLO_AGW_LICENSE_KEY=...          # SOLO_<ISTIO|KGATEWAY|AGW|KAGENT|AGENTREGISTRY>_LICENSE_KEY
```

`config/enterprise.env` holds the enterprise charts, versions and kinds;
`scripts/lib.sh` promotes them over the OSS pins for each product set to
`enterprise`. The stories, checks and tour are the same on both editions;
where a check needs an enterprise feature, OSS counts it as skipped and
names the feature.

## What Solo Enterprise adds in this lab

Running config on Solo Enterprise, each with the check that proves it, next
to what the OSS edition of the lab does instead. "Per Solo" is Solo's own
comparison of the editions.

| Product | On Solo Enterprise, in this lab | OSS edition of the lab | Per Solo | Checked by |
| --- | --- | --- | --- | --- |
| agentgateway | Token rate limits per agent on the model route, counted by the Solo rate limiter, keyed on each agent pool's mesh identity (`advisor-desk` held to 2,000 tokens a minute) | none (agentgateway OSS has local limits per route, or a rate-limit server you run yourself) | [global request- and token-based rate limiting](https://docs.solo.io/agentgateway/latest/about/overview/) | `make bob-verify` |
| agentgateway | Spend budgets in dollars or tokens per API key (`EnterpriseAgentgatewayBudget`): the developer key $5 a day, key `capped` blocked | none | [LLM spend budgets](https://docs.solo.io/agentgateway/latest/about/overview/) | `make bob-verify` |
| kgateway | A web application firewall on the edge: SQL and script injection refused for every party's hostnames | none | [WAF](https://docs.solo.io/kgateway/latest/security/waf/overview/) | `make bob-verify` |
| kagent | The controller verifies each user's token against S&V's broker, and maps the token's groups to roles | trusted-proxy mode: the controller trusts the forwarded token, so mesh policy fences who may reach it (ARCHITECTURE.md) | [OIDC authentication, RBAC mapped to IdP groups](https://docs.solo.io/kagent/latest/about/) | `make bob-verify` |
| agentregistry | Signs users in itself, roles from the token's groups | behind the edge's SSO | [user access control](https://docs.solo.io/agentregistry/latest/about/oss-enterprise/) | sign in at https://registry.sterling.lab |

Both editions run the rest of the lab the same way: MCP federation and
per-tool authorization, token exchange and Cross App Access toward tools
(the lab's `xaa-relay` and `idtoken-exchange` serve its brokered identity
continuity, on either edition), prompt guards, provider failover, cost per
call, API keys and format translation, and MCP guardrails.

Solo Enterprise also offers, per Solo's pages, and this lab doesn't show yet:

- agentgateway: the Cost Management UI (spend dashboards, budgets, virtual
  keys), the advanced UI and playground, on-behalf-of identity and claim
  propagation between agents and MCP tools, tool search and code mode for
  MCP, composable MCP servers, Microsoft Purview DLP, SAML subject
  assertions for Cross App Access
  ([overview](https://docs.solo.io/agentgateway/latest/about/overview/)).
- kgateway: the built-in rate-limit server, external auth with OPA, staged
  transformations ([overview](https://docs.solo.io/kgateway/latest/about/overview/)).
- kagent: multicluster agent management, controller-per-namespace
  multitenancy, the execution-flow UI, STS token exchange and AccessPolicy
  ([about](https://docs.solo.io/kagent/latest/about/)).
- agentregistry: artifact approval, audit logging, runtime MCP
  authorization, discovery across clouds
  ([OSS and Enterprise](https://docs.solo.io/agentregistry/latest/about/oss-enterprise/)).
- Istio: FIPS and long-term-support builds, multicluster peering, the Solo
  UI ([enterprise features](https://docs.solo.io/istio/latest/about/images/enterprise/)).

## Licences

Trial keys are scoped to a product (`product` in the key). What each product
does with a key it doesn't accept:

| Product | With a key it doesn't accept |
| --- | --- |
| Istio | istiod exits (`LICENSE ERROR`, `license loading failed`). With no key it runs, without enterprise features such as peering. An empty `SOLO_ISTIO_LICENSE_KEY` falls back to `SOLO_LICENSE_KEY`, so leave both empty to run it unlicensed |
| kgateway | logs `license validation failed` and runs; Solo warns of degraded or disabled features |
| agentgateway | logs `license validation failed` and runs; same warning |
| kagent | logs `kagent enterprise license missing or invalid` and runs |
| agentregistry | logs `agentregistry enterprise license missing or invalid` and runs |

## Versions

| Product | OSS | Enterprise |
| --- | --- | --- |
| [Istio](https://docs.solo.io/istio/) | upstream 1.31.1 | Solo Enterprise for Istio `1.31.1-solo` |
| [kgateway](https://docs.solo.io/kgateway/) | 2.4.5, GatewayClass `kgateway` | Solo Enterprise for kgateway 2.3.5, `enterprise-kgateway` |
| [agentgateway](https://docs.solo.io/agentgateway/) | 1.5.0, `agentgateway` | Solo Enterprise for agentgateway v2026.9.2, `enterprise-agentgateway` |
| [kagent](https://docs.solo.io/kagent/) | 0.10.2 + `tools/kagent` | Solo Enterprise for kagent 0.5.9 + `tools/kagent`'s Go ADK |
| [agentregistry](https://docs.solo.io/agentregistry/) | 0.4.0, behind edge SSO | agentregistry-enterprise 2026.9.0, its own sign-in |

## What changes, per product

### Istio

Solo-built images (`global.hub`/`tag`), the licence on istiod, peering
enabled. Nothing else.

### kgateway

- GatewayClass `enterprise-kgateway` (templated everywhere).
- Kubernetes 1.37 refuses the `EnterpriseKgatewayTrafficPolicy` CRD as over
  its CEL cost budget. Layer 30 installs a local copy of the CRD chart
  without the rules it names (format checks on durations, sizes and
  rate-limit entries).
- Enterprise kgateway and agentgateway both ship Solo's ext-auth,
  rate-limit and WAF CRDs. A CRD belongs to one helm release, so the first
  installed (kgateway, layer 30) keeps them and layer 40 leaves them out.

### agentgateway

- Parameters and policy kinds (`EnterpriseAgentgatewayParameters`,
  `EnterpriseAgentgatewayPolicy`), templated, ReferenceGrants included.
- The controller is `enterprise-agentgateway` (labels and ServiceAccount):
  S&V's mesh policy names it by release (xDS port, Keycloak's JWKS caller).

### kagent

0.5.9 keeps OSS kagent's agent model: SandboxAgents on Agent Substrate 0.0.9
(layer 50), the same CRDs and the same controller API, so the agents and
checks carry over.

- **Authentication:** the controller verifies every token against S&V's
  Keycloak (`oidc.*`), where OSS trusts the forwarded token. Roles come from
  the `groups` claim (`platform-admins` admin, `advisors` writer); the
  Observatory's client reads Substrate status. `oidc.skipOBO` passes the
  user's own token to agents, as the RFC 8693 exchange at the waypoint needs.
- **Go ADK:** `tools/kagent`'s build, pinned by digest
  (`GOLANG_ADK_IMAGE_DIGEST`): SandboxAgents call the controller back with
  the caller's credential.
- **Beyond the chart** (`platform/60-kagent/enterprise.yaml`): the licence
  Secret; a read on GatewayClasses, which the controller watches and the
  chart's namespace-scoped roles leave out; and an edge route for `/api`,
  which the release's UI image proxies to `127.0.0.1:8083` in its own pod.
- Agents get the caller's access token, not the ID token. Cross App Access
  doesn't need it: the egress gateway gets the ID token itself
  (IDENTITY-FLOWS.md, section 2).
- The management UI chart isn't installed; the chart's own UI runs behind
  the edge's SSO, as on OSS.

### agentregistry

- Signs people in itself (`oidc.*`): the UI runs PKCE as the public client
  `agentregistry-ui`, the server validates tokens issued to `agentregistry`,
  roles from `groups` (`platform-admins` superuser). The edge only routes:
  no OAuth2 filter, and `/mcp` goes to its MCP port.
- Trusts the lab CA from trust-manager's bundle; its ClickHouse gets a
  password of its own.
- Its database, ClickHouse and collector admit only the server's identity.
- It runs as root, so its namespace has no `restricted` PSA.
- Not wired: its kagent runtime (it deploys agents through kagent's REST API
  with a client-credentials client, configured with `arctl`).

The patches under `tools/` are independent of edition. Drop each one when
its upstream change ships.
