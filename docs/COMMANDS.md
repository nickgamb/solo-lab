# Commands

Terminal commands for working with the running lab. Every command targets the
lab's context explicitly (`--context kind-solo-lab`), so it's safe to run with
another cluster selected. To skip the flag, select the lab once:

```bash
kubectl config use-context kind-solo-lab
```

The `make` targets are in the [README](../README.md#make-targets); the commands behind each Observatory screen, as a card, in [cards/under-the-hood.html](cards/under-the-hood.html). Commands
that need the lab's helpers (`lab_secret`, `port_forward`) run under bash from
the repo root: `bash -c '. scripts/lib.sh; ...'`.

## The cluster

| Command | Shows |
| --- | --- |
| `make status` | nodes, pods that aren't running, the active IdP, URLs and sign-ins |
| `kubectl --context kind-solo-lab get nodes -L topology.kubernetes.io/zone` | the kind nodes and their zones |
| `kubectl --context kind-solo-lab get pods -A` | every pod |
| `kubectl --context kind-solo-lab get pods -A --field-selector=status.phase!=Running` | pods that aren't running |
| `kubectl --context kind-solo-lab get events -A --sort-by=.lastTimestamp \| tail -30` | the most recent events, cluster-wide |
| `kubectl --context kind-solo-lab get ns -L lab.solo.io/party,istio.io/dataplane-mode` | each namespace's party (zone) and whether it's in the ambient mesh |
| `kind get clusters` | kind clusters on this machine |
| `docker ps --format '{{.Names}}\t{{.Status}}'` | the kind nodes, registry mirrors, lab DNS and cloud-provider-kind |
| `curl -s localhost:5001/v2/_catalog \| jq` | images built by the lab, in the local registry |

## The mesh (Istio ambient)

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab get peerauthentication,authorizationpolicy -A` | every mTLS mode and allow/deny policy |
| `kubectl --context kind-solo-lab get authorizationpolicy <name> -n <ns> -o yaml` | who a policy admits, by SPIFFE principal |
| `kubectl --context kind-solo-lab get networkpolicy -A` | the egress fences (`no-internet`) |
| `istioctl --context kind-solo-lab ztunnel-config workloads` | every workload ztunnel knows, its address, node and waypoint |
| `istioctl --context kind-solo-lab ztunnel-config workloads --workload-namespace sv-agents` | the same, for one namespace |
| `istioctl --context kind-solo-lab ztunnel-config certificates --node solo-lab-worker` | the SPIFFE certificates ztunnel holds on a node |
| `istioctl --context kind-solo-lab waypoint list -A` | waypoints, what traffic they take, and whether they're programmed |
| `kubectl --context kind-solo-lab get serviceentry -A` | external hosts the mesh knows (the continuity controller's egress entries) |
| `kubectl --context kind-solo-lab -n istio-system logs ds/ztunnel --since=5m \| grep -i -E "deny\|rbac"` | recent connections ztunnel refused |

**Try a call as a real workload identity.** `make bob-verify` and `make tour`
create probe pods. `probe` (in `sv-agents`, `observability`, and `kagent` for
the tour) has its own ServiceAccount, so its own SPIFFE ID: some workload,
nobody special. `probe-bob-assistant` in `sv-agents` runs as Bob's agent's
ServiceAccount, to call as the agent.

```bash
kubectl --context kind-solo-lab -n sv-agents exec probe -- curl -s -m 5 -o /dev/null -w '%{http_code}\n' http://kps-prometheus.observability:9090/-/ready
```

`000` is the mesh refusing it: Prometheus names its callers, and this probe
isn't one.

```bash
kubectl --context kind-solo-lab -n sv-agents exec probe-bob-assistant -- python3 /tmp/p.py http://bob-workspace-mcp.sv-mcp:3000/mcp list --token "$(jq -r .access_token /tmp/bob.json)"
```

Bob's tools, as his agent (the token is from [Get Bob's tokens](#identity)).
`/tmp/p.py` is `tools/mcp-probe.py`: `list`, or `call <tool> '<json args>'`,
with `--token <jwt>` and `--header name=value`.

## Gateways

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab get gateway,httproute -A` | every gateway and route |
| `kubectl --context kind-solo-lab get httproute <name> -n <ns> -o jsonpath='{.status.parents[*].conditions}'` | whether a route was accepted, and why not |
| `kubectl --context kind-solo-lab get agentgatewaypolicy,agentgatewaybackend -A` | agentgateway's policies and backends |
| `kubectl --context kind-solo-lab get agentgatewaypolicy <name> -n <ns> -o jsonpath='{.status.ancestors[*].conditions}'` | whether a policy attached |
| `kubectl --context kind-solo-lab get agentgatewaybackend llm -n agentgateway-system -o jsonpath='{range .spec.ai.groups[*].providers[*]}{.name}{"\n"}{end}'` | the model chain, in failover order (what the Model Continuity tab edits) |
| `kubectl --context kind-solo-lab get gatewayextension,trafficpolicy -A` | the edge's SSO extensions and where they attach |
| `kubectl --context kind-solo-lab -n agentgateway-system logs deploy/ai-gateway -f \| grep request` | ai-gateway's access log, live: caller identity, route, JWT claims, status |
| `kubectl --context kind-solo-lab -n sv-mcp logs deploy/mcp-waypoint -f \| grep request` | the MCP waypoint's access log: tool, user, allowed or refused |
| `kubectl --context kind-solo-lab -n meridian logs deploy/meridian -f \| grep request` | Meridian's gateway |
| `kubectl --context kind-solo-lab -n kgateway-system logs deploy/edge -f` | the edge (Envoy) |
| `make llm LLM_PROVIDER=ollama` | point ai-gateway's model route at a provider |
| `make llm LLM_FALLBACK=anthropic` | add a second provider the model route fails over to |

**Use the firm's model from your laptop.** `https://llm.sterling.lab/v1` takes
OpenAI chat completions and Anthropic messages with the key in
`.lab/secrets.env`; prompt guards, failover and pricing apply as for agents.
Claude Code, for one:

```bash
ANTHROPIC_BASE_URL=https://llm.sterling.lab ANTHROPIC_AUTH_TOKEN="$(grep '^LLM_API_KEY=' .lab/secrets.env | cut -d= -f2-)" NODE_EXTRA_CA_CERTS="$HOME/.solo-lab/ca/ca.crt" claude
```

## Identity

| Command | Shows |
| --- | --- |
| `curl -s https://idp.sterling.lab/realms/sterling-vance/.well-known/openid-configuration \| jq` | S&V's issuer and endpoints (same for `idp.alice.lab/realms/alice`, `idp.ledgerline.lab/realms/ledgerline`, `idp.ops.lab/realms/ops`) |
| `grep -E 'KC_ADMIN_PASSWORD' .lab/secrets.env` | each Keycloak's admin password (user `admin`) |
| `kubectl --context kind-solo-lab -n sv-identity port-forward svc/keycloak 18080:80` | S&V broker's admin console at http://127.0.0.1:18080/admin (the edge publishes only the realm; same for `sv-workforce`, `ops-identity`, `alice-identity`, `ledgerline-identity`) |
| `make xaa-logs SINCE=2h` | the Cross App Access trail: both token requests, the ID-JAG's claims and every check, tokens redacted |
| `kubectl --context kind-solo-lab -n sv-identity logs deploy/keycloak --since=10m \| grep -i -E "claim\|IDENTITY_PROVIDER\|error"` | sign-in and broker errors (it names the claim or step that failed) |

**Get Bob's tokens** (the scripted browser sign-in the checks use: kagent's
SSO through the broker to S&V's own Keycloak; lab test accounts only):

```bash
bash -c '. scripts/lib.sh; . scripts/idp.sh; prefer_tier keycloak >&2; sso_token bob bob-demo' > /tmp/bob.json
```

`prefer_tier` drains the IdPs ahead of S&V's own Keycloak for the command and
restores the chain on exit.

**Decode a token's claims** (no verification; to see what it carries):

```bash
jq -r .access_token /tmp/bob.json | jq -R 'split(".")[1] | gsub("-";"+") | gsub("_";"/") | @base64d | fromjson'
```

## Identity continuity

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab get idc -A` | each IdentityContinuity and its active IdP |
| `kubectl --context kind-solo-lab get idc sterling-vance -n sv-identity -o jsonpath='{range .status.tiers[*]}{.name}: {.reason} {.message}{"\n"}{end}'` | each IdP's health (and break-glass) |
| `kubectl --context kind-solo-lab get idc sterling-vance -n sv-identity -o jsonpath='{.status.transitions}' \| jq` | the last 20 failovers and failbacks |
| `kubectl --context kind-solo-lab get events -n sv-identity --field-selector involvedObject.name=sterling-vance` | IdP health and failover events |
| `kubectl --context kind-solo-lab -n sv-identity logs -l app=continuity-controller --prefix -f` | the controller, both replicas (the leader does the work) |
| `kubectl --context kind-solo-lab get lease continuity.lab.solo.io -n sv-identity -o jsonpath='{.spec.holderIdentity}'` | which controller replica leads |
| `kubectl --context kind-solo-lab get authorizationpolicy -A -l continuity.lab.solo.io/tier -o custom-columns='NAMESPACE:.metadata.namespace,POLICY:.metadata.name,IDP:.metadata.labels.continuity\.lab\.solo\.io/tier,CUT BY:.metadata.annotations.continuity\.lab\.solo\.io/cut-by'` | every simulated outage in effect, wherever its IdP runs, and who cut it |
| `kubectl --context kind-solo-lab -n sv-egress delete authorizationpolicy continuity-partition-auth0` | end a simulated Auth0 outage |
| `kubectl --context kind-solo-lab -n sv-identity get cronjob,job -l app=continuity-sync` | the directory sync's schedule and runs |
| `kubectl --context kind-solo-lab -n sv-identity create job --from=cronjob/sterling-vance-profile-sync sync-now` | run the directory sync now |
| `kubectl --context kind-solo-lab -n sv-identity logs job/sync-now` | what it did (users by id only) |
| `kubectl --context kind-solo-lab get idc sterling-vance -n sv-identity -o jsonpath='{.status.sync}' \| jq` | the sync's last run, counts and detected attributes |
| `kubectl --context kind-solo-lab get idc sterling-vance -n sv-identity -o json \| jq -r '.status.tiers[] \| .name as $n \| .trust.checks[]? \| [$n, .name, .result] \| @tsv'` | each IdP's trust checks (callback, client authentication, PKCE, scopes, claims, assurance) |
| `kubectl --context kind-solo-lab annotate idc sterling-vance -n sv-identity --overwrite continuity.lab.solo.io/check-trust=$(date +%s)` | run the trust checks now |
| `make totp` | Bob's current one-time code at S&V's own Keycloak (`make totp EMPLOYEE=carol`) |

## Assurance rules and the gate

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab get svc -A -l continuity.lab.solo.io/assurance-gate` | the assurance gates (any Service with the label, whatever its name) |
| `kubectl --context kind-solo-lab get pods -n sv-identity -l app=assurance-gate -o wide` | the gate's replicas, spread across nodes |
| `kubectl --context kind-solo-lab get pdb assurance-gate -n sv-identity` | how many may be down at once |
| `kubectl --context kind-solo-lab get "$(kubectl --context kind-solo-lab api-resources -o name \| grep agentgatewaypolicies \| paste -sd, -)" -A -o custom-columns='NAMESPACE:.metadata.namespace,POLICY:.metadata.name,EXTAUTH:.spec.traffic.extAuth.backendRef.name,RULE:.spec.traffic.extAuth.grpc.contextExtensions.profile,FAILURE:.spec.traffic.extAuth.failureMode'` | every gateway policy, the external authorization it asks (the assurance gate, for which rule) and how it fails |
| `kubectl --context kind-solo-lab get validatingadmissionpolicy assurance-gate-fail-closed -o jsonpath='{.spec.validations[*].expression}'` | the admission rule that holds every policy asking a gate to FailClosed (its binding selects the gates by label) |
| `kubectl --context kind-solo-lab get referencegrant,authorizationpolicy -n sv-identity -l continuity.lab.solo.io/assurance-gate-caller` | the grants the Observatory added when enforcement was turned on at another gateway |
| `kubectl --context kind-solo-lab get wlp -n sv-identity` | each rule: criticality, mode, minimum (empty: the default rule's), phase, the IdP serving it |
| `kubectl --context kind-solo-lab get idc sterling-vance -n sv-identity -o jsonpath='{.spec.assurancePolicy}'` | the default rule: what every rule starts from |
| `kubectl --context kind-solo-lab get wlp advisor-workspace -n sv-identity -o jsonpath='{.status}' \| jq` | a rule's eligible IdPs, clients as the broker has them, and conditions |
| `kubectl --context kind-solo-lab get events -n sv-identity --field-selector involvedObject.kind=WorkloadProfile` | rules failing closed and recovering |
| `kubectl --context kind-solo-lab -n sv-identity logs -l app=assurance-gate --prefix -f \| grep '"msg":"decision"'` | the gate's decisions: rule, allow, deny or would-deny, why, the session's IdP and acr |
| `kubectl --context kind-solo-lab -n sv-mcp logs deploy/mcp-waypoint \| grep -o 'continuity.decision="[^"]*"'` | the decisions as the waypoint logged them |
| `kubectl --context kind-solo-lab patch wlp advisor-workspace -n sv-identity --type merge -p '{"spec":{"assurance":{"minimum":"AAL1"}}}'` | change what a rule requires (the gate applies it to the next request) |
| `kubectl --context kind-solo-lab patch wlp advisor-workspace -n sv-identity --type merge -p '{"spec":{"mode":"ReportOnly"}}'` | let its refusals through, logged as `would-deny`, to see a change's effect first (`Enforce` to enforce again) |
| `kubectl --context kind-solo-lab -n sv-identity port-forward svc/assurance-gate 19002:evaluate` then `curl -s localhost:19002/v1/evaluate -d '{"continuity": "sterling-vance", "session": {"idp": "contingency", "acr": "aal1"}}' \| jq` | what every rule decides for one sign-in, from the gate (port-forward: its mesh policy lets only the Observatory call it in-cluster) |
| `make continuity-verify` | the failover, kill switch and live-rule checks |

The kill switch itself is in [IDENTITY-CONTINUITY.md](IDENTITY-CONTINUITY.md#kill-switch).

## Agents and Agent Substrate

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab get sandboxagents,agents -A` | agents, and which run on Substrate |
| `kubectl --context kind-solo-lab get remotemcpservers,mcpservers,modelconfigs -A` | the tools and models agents use |
| `kubectl --context kind-solo-lab get sandboxagent bob-assistant -n sv-agents -o yaml` | Bob's agent: tools, approvals, forwarded headers |
| `kubectl --context kind-solo-lab get workerpools,actortemplates -A` | Substrate's worker pools and the agents' actor templates |
| `kubectl --context kind-solo-lab get pods -A -l ate.dev/worker-pool` | the worker pods (an actor's calls leave as its pool's ServiceAccount) |
| `kubectl --context kind-solo-lab -n kagent logs deploy/kagent-controller -f` | the kagent controller (A2A turns, sessions, Substrate resumes) |
| `kubectl --context kind-solo-lab -n ate-system logs deploy/ate-controller --since=10m` | Substrate's controller (actors starting and suspending) |
| `make tour` | every story end to end, with where to look in the Observatory |

## Observability

| Command | Shows |
| --- | --- |
| `kubectl --context kind-solo-lab -n observability port-forward svc/kps-prometheus 19090:9090` | Prometheus at http://127.0.0.1:19090 |
| `curl -s --get http://127.0.0.1:19090/api/v1/query --data-urlencode 'query=sum by (source_principal,destination_workload) (istio_tcp_connections_opened_total{reporter="destination"})' \| jq '.data.result[] \| [.metric.source_principal, .metric.destination_workload, .value[1]]'` | who has connected to what, by SPIFFE identity |
| `kubectl --context kind-solo-lab -n observability logs deploy/otel-collector --since=5m \| grep -i "exporting failed"` | the collector failing to deliver (to the Observatory, Tempo or Prometheus) |
| `kubectl --context kind-solo-lab -n observatory logs deploy/observatory -f` | the Observatory's server |
| `kubectl --context kind-solo-lab auth can-i --list --as=system:serviceaccount:observatory:observatory` | what the Observatory may do as itself: read, and impersonate one admin identity |
| `kubectl --context kind-solo-lab auth can-i --list --as=observatory:admin --as-group=observatory:observatory-admins -n sv-identity` | what its signed-in admins may change there |
| `kubectl --context kind-solo-lab get wlp advisor-workspace -n sv-identity --show-managed-fields -o jsonpath='{range .metadata.managedFields[*]}{.manager}{"\t"}{.time}{"\n"}{end}'` | who last wrote an object (`observatory`: saved from the Observatory, as the signed-in admin); any kind and name |

Grafana and Kiali are at https://grafana.ops.lab and https://kiali.ops.lab
(`ops` / `ops-demo`). The Observatory's local development loop is in
[OBSERVATORY.md](OBSERVATORY.md#local-development).

## Fixing things

| Command | Does |
| --- | --- |
| `kubectl --context kind-solo-lab describe pod <pod> -n <ns>` | why a pod isn't starting (events at the bottom) |
| `kubectl --context kind-solo-lab logs <pod> -n <ns> --previous` | the logs of a container that crashed |
| `kubectl --context kind-solo-lab rollout restart deploy/<name> -n <ns>` | restart a workload |
| `make layer-NN` | re-apply one layer (idempotent); e.g. `make layer-80` restores the S&V mesh policy |
| `make preflight` | tools, Docker memory and inotify limits (Docker VMs: reset when the VM restarts), free ports, `*.lab` DNS |
| `make reset` | rewind the demos without a rebuild |
| `make down && make up` | rebuild the cluster; caches and the CA survive |
