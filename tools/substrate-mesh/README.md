# Agent Substrate 0.0.9 in a service mesh (upstream PR candidates)

On an ambient-meshed cluster, stock 0.0.9 can't run a single actor: the
golden snapshot never gets past readyz. Fixing that, and giving each agent the
workload identity every policy in this lab keys on, takes three patches
against v0.0.9, each its own upstream PR.

## What was measured (kind, Istio 1.31 ambient, worker pods meshed)

- **readyz never passes.** ateom probes the actor at `169.254.17.2:80` from the
  worker pod. Locally generated, so ambient's in-pod `OUTPUT` rule sends it to
  ztunnel (15001), which never reaches the veth: `connection refused`, 21k
  attempts in 30s. With the pod taken out of the mesh the golden builds at once.
- **Actor egress leaves without an identity.** The actor's packets are
  forwarded (veth `ateom0` → pod `eth0`, masqueraded), so ambient treats them as
  inbound (`PREROUTING` → 15006); they never get mTLS. Unmeshed, every call to
  the fenced kagent controller is refused (`EOF` on `/api/tasks`).
- **Actor DNS breaks** under ztunnel's DNS capture: masqueraded replies land in
  conntrack zone 1 and never match (`[UNREPLIED]`).
- **Inbound bypasses the mesh.** The router's plaintext to the worker's `:80`
  is DNAT'd in `PREROUTING`, racing ambient's own redirect at the same hook;
  STRICT mTLS and authorization never see it. A meshed caller fares worse: its
  ztunnel delivers to a local `:80` where nothing listens.
- **Every worker is `ns/<pool ns>/sa/default`.** A pool can't pick a
  ServiceAccount or annotate its pods, and worker selectors match labels across
  namespaces.

## The patches

| Patch | Component | What |
| --- | --- | --- |
| `0001` | WorkerPool API, atecontroller | `spec.template.{serviceAccountName,labels,annotations}` → worker pods. `ate.dev/worker-pool` is reserved (CEL). CRD regenerated (`crds/`) |
| `0002` | ateapi | a pool with its own ServiceAccount only runs actors from ActorTemplates in its namespace (selectors are cluster-wide; identity must not be borrowable). Default-SA pools stay shareable |
| `0003` | ateom-gvisor | inbound `:80` relayed to the actor by ateom (a local socket, so whatever terminates or authorizes inbound keeps working) instead of the `PREROUTING` DNAT; ateom's own connections to the actor pinned by a no-op DNAT at `dstnat-50` (the kernel applies a connection's first NAT binding only, so later redirects never see them) |

With those, a pool opts into the mesh with two pod annotations (no patch):

```yaml
template:
  serviceAccountName: bob-assistant
  annotations:
    istio.io/reroute-virtual-interfaces: ateom0   # actor TCP egress -> ztunnel 15001, as the worker
    ambient.istio.io/dns-capture: "false"         # actor DNS forwarded as-is
```

Result, from ztunnel's access logs: actor egress arrives at the controller,
tools and gateways as `spiffe://cluster.local/ns/sv-agents/sa/bob-assistant`;
the router reaches the worker over HBONE as `ns/ate-system/sa/atenet-router`.

## Build

`./build.sh` → `localhost:5001/kagent-dev/substrate/<component>:v0.0.9-lab.1`.
ateapi, atecontroller and ateom-gvisor are built from the patched source the
way `ko build` makes them (static binary at `/ko-app/<name>` on distroless
static, same env and entrypoint). atelet and atenet are mirrored from the
release by digest (`released-digests.env`) so the chart's single
`image.registry`/`image.tag` can point here. It also refreshes
`crds/ate.dev_workerpools.yaml`, which `platform/50-substrate` swaps into the
release's CRD chart.

**Upstream:** open against `kagent-dev/substrate` main (ateom's comments plan an
AgentGateway egress phase; 0003 leaves egress alone, so it composes). Once
released, drop `image` from `platform/50-substrate/values.yaml`, the CRD
swap, and the `ateomImage` overrides.
