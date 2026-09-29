# Observatory

A live view of the whole lab for platform admins: what runs where, what calls
what, the traffic on each call, and the sign-in chain. It reads the cluster at
runtime and assumes nothing about this lab's names, so another cluster
renders the same way.

Source: `apps/observatory` (Go server in `server/`, React UI in `web/`).
Install: `platform/90-observatory` (`make layer-90`).

## Seeing it work

[![Solo.io Observatory walkthrough on YouTube](https://img.youtube.com/vi/Y3P4a7HRvVs/maxresdefault.jpg)](https://youtu.be/Y3P4a7HRvVs)

A three-minute walkthrough of all three tabs, [on YouTube](https://youtu.be/Y3P4a7HRvVs).

`make tour` drives every story end to end with real traffic, one scene at a
time, and prints where to look. The script for presenting it is the
[Observatory demo card](cards/observatory.html).

## Access

`https://observatory.ops.lab`, user `ops` / `ops-demo`.

Sign-in uses the Observatory's own Keycloak (namespace `ops-identity`, realm
`ops`), not the parties' IdPs. The edge runs the OAuth2 flow
(`GatewayExtension observatory-sso`) and forwards the access token; the server
verifies it again (issuer, audience `observatory`) and requires group
`observatory-admins`.

## Tabs

### Topology

Every workload, drawn as a layered diagram read left to right.

![Topology, Identity view](images/observatory-topology-identity-view.jpg)

| Element | Meaning |
| --- | --- |
| Lane | a zone (party): namespaces with the same party label; the bar across its top is its live status |
| Column | a call stage: callers sit left of what they call; headings name what each stage mostly holds |
| Front door (left pillar) | the internet-facing kgateway; each zone's way in is a line out of it |
| Outside the lab (right column) | model providers and external services |
| Tray | an Agent Substrate worker pool around the agents it runs; lit bays are busy workers |
| Wire | a call. Colour is the protocol, thickness is traffic, moving dots mean traffic in the last window; dashed grey is configured but idle |
| Badge on a wire | a hop the call passes through: a waypoint, the edge, the Substrate router |
| Stack ("4 agents") | look-alike nodes with the same neighbours; open it to list or unfold them |

Interactions:

- Hover a node: it, its neighbours and its pool stay lit.

  ![Hovering ai-gateway in the Identity view](images/observatory-topology-hover-ai-gateway.jpg)

- Click a node: details (summary, tools, pods, mesh identity, connections,
  configuration, and only that node's traffic). **Advanced** opens the object's
  live YAML in an editor; Apply does a server-side dry run, shows the diff,
  then applies it as you.

  ![Details for ai-gateway: its configuration objects and its traffic](images/observatory-details-ai-gateway.jpg)

- Click a zone's status bar: every workload in the zone by kind, including
  the idle ones the map folds away.
- Hover a wire: what it stands for, its rates and its recent calls.
- Views (top left): **All**; **Identity** (IdPs, SSO, token exchange,
  gateways that check tokens, and where each agent's token goes);
  **Agents & tools** (agents, gateways, MCP servers, models);
  **Cross-party** (only calls that cross a zone). Each view re-lays out.
- Product rail (right): hover or pin a Solo product to light every instance:
  tiles, badges on wires, Substrate trays, and a count on the status bar of a
  zone whose instances are folded away. Each product links to its docs on
  [docs.solo.io](https://docs.solo.io).
- Export (map controls, bottom left): saves the whole map, in its current
  view, as a PNG, such as [the All view](images/observatory-export-topology-all.jpg) or
  [the Identity view](images/observatory-export-topology-identity.jpg).

### Traffic

Every request any gateway saw, plus runtime events, newest first.

![Traffic](images/observatory-traffic.jpg)

| Kind | Source |
| --- | --- |
| `http` `mcp` `a2a` `llm` `oidc` | gateway access logs (agentgateway, kgateway), over OTLP |
| `lifecycle` | pods created, ready, not ready, restarted |
| `substrate` | Substrate actors starting, suspending, resuming |
| `continuity` | sign-in failovers, and outages cut or restored from the Observatory |

Filters: kind, outcome, free text (user, workload, tool, path), **Hide tool
discovery** (MCP `initialize` and `tools/list`), **Carries a token**, and Pause.
Expand a row for the parsed record or its raw JSON.

A request that carried a token shows a key badge. Expanded, each token is
listed with its type (access token, ID token, ID-JAG), whether the gateway
verified it, and its claims: subject, user, issuer, audience, `azp`, scope,
groups, `act`, issued and expiry times, and the full claim set on demand.

Raw tokens never reach the Observatory's store or the browser. Gateways log
only the verified claims (`jwt`; agentgateway redacts the raw token), and
any JWT found in another log field is decoded and replaced with a SHA-256
fingerprint before the record is kept.

### Identity Continuity

The sign-in chain for each `IdentityContinuity`, the outage button and the
rule builder. See [IDENTITY-CONTINUITY.md](IDENTITY-CONTINUITY.md#in-the-observatory).

## Where the data comes from

| Data | Source |
| --- | --- |
| Nodes and declared edges | dynamic informers over workloads, Services, Gateway API routes, agentgateway backends and policies, kagent Agents, SandboxAgents, MCP servers, ModelConfigs, Substrate WorkerPools, Istio policies, CNPG clusters, IdentityContinuity; rediscovered periodically |
| Observed edges, L4 rates | Prometheus: `istio_tcp_connections_opened_total{reporter="destination"}` from ztunnel |
| Requests, token claims | OTLP/HTTP logs on port 4318, from the platform collector |
| Substrate workers and actors | kagent's `/api/substrate/status` (actor state changes also go to Traffic) |

How the graph is built:

- A workload's Deployment merges into the resource that owns it (Gateway,
  Agent, SandboxAgent, MCPServer, WorkerPool).
- Edges come from HTTPRoutes and backends, RemoteMCPServer and ModelConfig
  URLs, policy and SSO references, URLs in env vars, and observed mesh
  connections.
- A call to a Service enrolled in a waypoint is drawn through that waypoint.
- A call to one of the edge's own hostnames is drawn to the service behind
  that hostname, with the edge as a badge.
- Solo products are recognised from images and gateway classes.

The browser gets everything over one Server-Sent Events stream (`/api/stream`:
`graph`, `stats`, `traffic`, `continuity`, `substrate`).

## Access model

- The edge signs the admin in (realm `ops`) and forwards the access token.
  The server verifies it again: signature, issuer, audience `observatory`,
  issued to the edge's client (`azp`), an access token (`typ: Bearer`, not an
  ID token), and group `observatory-admins`. The live stream closes when that
  token expires; the browser reconnects through the edge, which refreshes
  the session or asks the admin to sign in again.
- Writes must come from the Observatory's own page: a request another site
  sends with the admin's session cookie is refused (`Sec-Fetch-Site` and
  `Origin`), and a write needs a JSON or YAML body, which a plain HTML form
  can't send. Every response carries a Content-Security-Policy that loads
  only the Observatory's own code and forbids framing.
- The server runs as ServiceAccount `observatory` with ClusterRole
  `observatory-read`: read the lab's resources (no Secrets), and impersonate
  exactly one identity.
- Every write (Apply, rule edits, secrets, outages) is made by impersonating
  user `observatory:admin` in group `observatory:observatory-admins`, with the
  signed-in person's name as the extra `observatory-user`. RBAC names all
  three (`platform/90-observatory/rbac.yaml`), so no request can make the
  Observatory any other user or group. The group is bound to `cluster-admin`.
  With API server audit logging on, each write records the person in
  `impersonatedUser.extra`.
- Client secrets entered in the rule builder are write-only.
- The namespace is STRICT mTLS; only the edge (8080), the collector (4318)
  and its Keycloak may call in.

## Telemetry wiring

- `platform/90-observatory/telemetry.yaml`: access-log policies on ai-gateway,
  the S&V MCP waypoint and Meridian's gateway (agentgateway), and on the edge
  (kgateway ListenerPolicy), all to the OTel collector. agentgateway sends to
  the collector's Service as a backend, so the export carries the gateway's
  mesh identity, which the collector's policy requires.
- `platform/20-observability/otel-collector.yaml`: the collector's logs
  pipeline (`k8sattributes`) exports to `observatory.observatory.svc:4318`.

## Server configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `OIDC_ISSUER` | | issuer of admin tokens |
| `OIDC_JWKS_URL` | | JWKS the server verifies against (the Keycloak Service, in-cluster) |
| `OIDC_AUDIENCE` | `observatory` | required audience |
| `OIDC_CLIENT_ID` | `observatory` | required `azp`: the client the edge signs in with |
| `ADMIN_GROUP` | `observatory-admins` | required group |
| `PROMETHEUS_URL` | | Prometheus for mesh edges and L4 rates (optional) |
| `KAGENT_URL` | | kagent controller for Substrate status (optional) |
| `PARTY_LABEL` | `lab.solo.io/party` | namespace label that defines zones |
| `LISTEN`, `OTLP_LISTEN` | `:8080`, `:4318` | UI/API and OTLP listeners |
| `OBSERVATORY_DEV_USER` | | local development only: skip sign-in as this user (ignored in a cluster) |

## Using it on another cluster

Zones come from a namespace label; nothing else is lab-specific.

```yaml
metadata:
  labels:
    lab.solo.io/party: acme            # the zone id (PARTY_LABEL)
  annotations:
    observatory.solo.io/party-name: "Acme Corp"
    observatory.solo.io/party-order: "10"   # optional
```

Unlabelled namespaces are grouped as `cluster` and left off the map. Without
Prometheus the map shows declared edges only; without OTLP logs the Traffic
tab shows runtime events only.

## Local development

Runs against your current kubeconfig context, as you.

```bash
cd apps/observatory/web && npm ci && npm run build        # writes ../server/web
cd ../server && go build -o /tmp/obs .                    # Go 1.27
kubectl --context kind-solo-lab -n observability port-forward svc/kps-prometheus 19090:9090 &
OBSERVATORY_DEV_USER=$USER PROMETHEUS_URL=http://127.0.0.1:19090 \
  LISTEN=127.0.0.1:8080 OTLP_LISTEN=127.0.0.1:14318 /tmp/obs
```

Open http://127.0.0.1:8080. `npm run dev` in `web/` serves the UI with hot
reload and proxies `/api` to that server. `/tmp/obs graph` prints the derived
graph as JSON and exits.

Gateway access logs go to the in-cluster Observatory, not a local one, so
the local Traffic tab has runtime events only. To test log parsing, POST an
OTLP/JSON logs body to `http://127.0.0.1:14318/v1/logs`.

Without a local Go toolchain, build in a container:

```bash
docker run --rm -v "$PWD":/src -w /src -e GOOS=darwin -e GOARCH=arm64 golang:1.27-alpine go build -o obs .
```

Tests: `go test ./...` in `server/`, `npm run build` (type-check) in `web/`.

Deploy a change with `make layer-90`. The image tag is a hash of the source,
so any change rolls out.
