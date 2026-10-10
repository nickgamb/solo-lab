import type { EdgeKind, LabEdge, LabNode } from '../api'

// mapview turns the live graph into what the map draws, following how
// service maps stay readable (Kiali, Gloo Mesh, Datadog, New Relic):
//
//   - waypoints ride on the arrow as a badge, not as an extra hop (the
//     mesh reports the same connection at the waypoint and at the workload)
//   - control-plane and telemetry plumbing is left to the details panel
//   - workloads nothing calls and that call nothing collapse into one
//     "services" node per party
//   - look-alike nodes (same party, role and neighbours) stack with a count
//
// The graph behind it is untouched: details, traffic and pulses keep using
// the real node and edge ids (each drawn edge carries the ids it stands for).

export type MapEdge = LabEdge & { of: string[]; via: string[] }

// Views: each keeps what answers one question and re-lays out without the
// rest. Derived from edge kinds and policy links, never from names.
export type Lens = 'all' | 'identity' | 'agents' | 'cross'
export const LENSES: { id: Lens; label: string; hint: string }[] = [
  { id: 'all', label: 'All', hint: 'every workload and call' },
  { id: 'identity', label: 'Identity', hint: 'who authenticates whom, and where tokens go: IdPs, SSO, token exchange, policy enforcement' },
  { id: 'agents', label: 'Agents & tools', hint: 'agents, gateways, MCP servers and models: the agentic path' },
  { id: 'cross', label: 'Cross-party', hint: 'only calls that cross a trust boundary' },
]

const TOKEN_PATH: EdgeKind[] = ['a2a', 'mcp', 'http']
const AGENTIC: EdgeKind[] = ['a2a', 'mcp', 'llm']

function lensed(lens: Lens, nodes: LabNode[], edges: LabEdge[]): { nodes: LabNode[]; edges: LabEdge[] } {
  if (lens === 'all') return { nodes, edges }
  const byId = new Map(nodes.map(n => [n.id, n]))
  let keep: LabEdge[] = []
  if (lens === 'identity') {
    // the user's identity, end to end: every IdP link (SSO, JWT validation,
    // token exchange, ext-auth), who hands each agent the user's token
    // (back through the dispatcher and the UI to the edge), and everywhere
    // that token, or what it's exchanged for, goes next
    const idLink = (e: LabEdge) => e.kind === 'oidc' || e.label === 'policy' || e.label === 'SSO'
    const agents = nodes.filter(n => n.kind === 'agent').map(n => n.id)
    const walked = new Set<string>()
    const walk = (start: string[], forward: boolean, depth: number) => {
      let level = start, seen = new Set(start)
      for (let d = 0; d < depth && level.length; d++) {
        const nx: string[] = []
        for (const id of level) for (const e of (forward ? fwd(id) : back(id))) {
          const t = forward ? e.target : e.source
          // through the entry gateway, follow calls on to services, not to every UI it fronts
          const via = byId.get(id)
          if (via?.kind === 'gateway' && via.products?.includes('kgateway') && byId.get(t)?.kind === 'ui' && forward) continue
          walked.add(e.id)
          if (!seen.has(t)) { seen.add(t); nx.push(t) }
        }
        level = nx
      }
    }
    const fwd = (id: string) => edges.filter(e => e.source === id && [...TOKEN_PATH, 'mesh'].includes(e.kind))
    const back = (id: string) => edges.filter(e => e.target === id && (e.kind === 'a2a' || e.kind === 'http'))
    walk(agents, true, 5)
    walk(agents, false, 3)
    keep = edges.filter(e => idLink(e) || walked.has(e.id))
    // a gateway that checks tokens against an IdP is on the identity path:
    // the calls it checks, and the tools it reaches for them (token exchange)
    const enforcers = new Set(edges.filter(idLink).map(e => e.source))
    const onPath = new Set([...agents, ...keep.flatMap(e => [e.source, e.target])])
    for (const e of edges) {
      if (keep.includes(e) || e.kind === 'control' || e.kind === 'telemetry') continue
      if (enforcers.has(e.target) && onPath.has(e.source)) keep.push(e)
      else if (enforcers.has(e.source) && (e.kind === 'mcp' || e.kind === 'a2a')) keep.push(e)
    }
  } else if (lens === 'agents') {
    // agent, model and tool calls, followed on through the gateways and
    // waypoints they pass until they land (not to UIs or IdPs on the way),
    // and back to where a person starts the agent
    keep = edges.filter(e => AGENTIC.includes(e.kind))
    let up = [...new Set(keep.filter(e => e.kind === 'a2a').map(e => e.source))]
    for (let d = 0; d < 2 && up.length; d++) {
      const nx: string[] = []
      for (const id of up) for (const e of edges) if (e.target === id && e.kind === 'http' && !keep.includes(e)) { keep.push(e); nx.push(e.source) }
      up = nx
    }
    let level = [...new Set(keep.map(e => e.target))]
    const seen = new Set(level)
    for (let d = 0; d < 3 && level.length; d++) {
      const nx: string[] = []
      for (const id of level) for (const e of edges) {
        if (e.source !== id || !['http', 'mesh', 'mcp'].includes(e.kind)) continue
        const t = byId.get(e.target)
        if (!t || t.kind === 'ui' || t.kind === 'idp' || t.kind === 'fabric' || t.kind === 'db') continue
        // through the entry gateway, only on to the gateways and tools it fronts
        const src = byId.get(id)
        if (src?.kind === 'gateway' && src.products?.includes('kgateway') && !['gateway', 'waypoint', 'mcp'].includes(t.kind)) continue
        if (!keep.includes(e)) keep.push(e)
        if (!seen.has(e.target)) { seen.add(e.target); nx.push(e.target) }
      }
      level = nx
    }
  } else if (lens === 'cross') {
    const party = (id: string) => { const n = byId.get(id); return !n || n.kind === 'external' || n.kind === 'llm' ? `out:${id}` : n.group }
    keep = edges.filter(e => e.kind !== 'control' && e.kind !== 'telemetry' && party(e.source) !== party(e.target))
  }
  // a call that reaches a waypoint lands behind it: keep the onward hop too
  for (const e of [...keep]) {
    if (byId.get(e.target)?.kind !== 'waypoint') continue
    for (const o of edges) if (o.source === e.target && o.kind === 'mesh' && !keep.includes(o)) keep.push(o)
  }
  const ids = new Set(keep.flatMap(e => [e.source, e.target]))
  return { nodes: nodes.filter(n => ids.has(n.id)), edges: keep }
}

const SHOWN: EdgeKind[] = ['http', 'mcp', 'a2a', 'llm', 'oidc', 'mesh', 'db']
const RANK: Record<string, number> = { down: 3, warn: 2, idle: 1, ok: 0 }

export function mapView(nodes: LabNode[], edges: LabEdge[], expanded: Set<string>, lens: Lens = 'all') {
  nodes = nodes.filter(n => n.group !== 'party:cluster' || n.kind === 'external' || n.kind === 'llm')
  const everyone = new Map(nodes.map(n => [n.id, n]))
  // which agents each worker pool runs, before "runs on" links are dropped
  const pools: Record<string, string[]> = {}
  for (const e of edges) if (e.kind === 'substrate') (pools[e.target] ??= []).push(e.source)
  edges = throughEdge(nodes, edges)
  edges = throughPools(nodes, edges, pools)
  ;({ nodes, edges } = lensed(lens, nodes, edges))
  const ids = new Set(nodes.map(n => n.id))
  let es: MapEdge[] = edges.filter(e => SHOWN.includes(e.kind) && ids.has(e.source) && ids.has(e.target))
    .map(e => ({ ...e, of: (e as MapEdge).of ?? [e.id], via: (e as MapEdge).via ?? [] }))

  // 1. waypoints as badges: X → waypoint → Y becomes X → Y "via waypoint"
  const wps = new Set(nodes.filter(n => n.kind === 'waypoint').map(n => n.id))
  if (wps.size) {
    const next: MapEdge[] = []
    for (const e of es) {
      if (wps.has(e.target)) {
        const outs = es.filter(o => o.source === e.target && !wps.has(o.target) && o.kind !== 'oidc')
        // traffic counts only the caller's own hop into the waypoint; the
        // waypoint's onward hop is shared by every caller
        for (const o of outs) next.push({ ...e, id: `${e.source}>${o.target}`, target: o.target, of: [...e.of], via: [...e.via, e.target] })
        continue
      }
      if (wps.has(e.source)) continue // shown through the badge above
      next.push(e)
    }
    es = dedupe(next)
    nodes = nodes.filter(n => !wps.has(n.id))
  }

  // 2. test tools, and workloads with nothing on the map, collapse into one
  // node per party (worker pools stay: agents point at them by badge)
  const tools = new Set(nodes.filter(n => n.kind === 'tool').map(n => n.id))
  es = es.filter(e => !tools.has(e.source) && !tools.has(e.target))
  const linked = new Set(es.flatMap(e => [e.source, e.target]).concat(nodes.filter(n => n.kind === 'substrate').map(n => n.id)))
  const idle = new Map<string, LabNode[]>()
  for (const n of nodes) if (!linked.has(n.id) && n.kind !== 'external' && n.kind !== 'llm') (idle.get(n.group) ?? idle.set(n.group, []).get(n.group)!).push(n)
  const folded: LabNode[] = []
  if (lens !== 'all') {
    const gone = new Set([...idle.values()].flat().map(n => n.id))
    nodes = nodes.filter(n => !gone.has(n.id))
    idle.clear()
  }
  for (const [group, list] of idle) {
    if (!list.length) continue
    list.sort((a, b) => a.label.localeCompare(b.label))
    folded.push({ id: `services:${group}`, kind: 'cluster', group, label: `${list.length} idle`, sub: list.map(n => n.label).join(', '),
      status: worst(list), products: [...new Set(list.flatMap(n => n.products ?? []))], pods: list.flatMap(n => n.pods ?? []),
      related: list.flatMap(n => (n.ref ? [n.ref] : [])), summary: { members: list.map(n => n.id), note: 'running, with no request path on the map' } })
  }
  const hidden = new Set(folded.flatMap(f => f.summary!.members as string[]))
  nodes = nodes.filter(n => !hidden.has(n.id))

  // 3. look-alike nodes stack; the idle ones live in their zone's status bar
  const poolOf = new Map(Object.entries(pools).flatMap(([p, as]) => as.map(a => [a, p] as const)))
  const st = stack(nodes, es, expanded, poolOf)
  // 4. worker pools become trays around the agents they run
  const present = new Set(st.nodes.map(n => n.id))
  const into = new Map<string, string>()
  for (const n of st.nodes) for (const m of [n.id, ...((n.summary?.members as string[] | undefined) ?? [])]) into.set(m, n.id)
  const trays: Record<string, string[]> = {}
  for (const [pool, agents] of Object.entries(pools)) {
    const shown = [...new Set(agents.map(a => into.get(a)).filter((x): x is string => !!x && present.has(x)))]
    if (shown.length && everyone.has(pool)) trays[pool] = shown
  }
  const trayed = new Set(Object.keys(trays))
  return { nodes: st.nodes.filter(n => !trayed.has(n.id)), edges: st.edges.filter(e => !trayed.has(e.source) && !trayed.has(e.target)),
    trays: Object.fromEntries(Object.entries(trays).map(([id, m]) => [id, { node: everyone.get(id)!, members: m }])),
    folded: Object.fromEntries(folded.map(f => [f.group, f])) as Record<string, LabNode> }
}

export type Tray = { node: LabNode; members: string[] }

// throughEdge: a call that leaves through the internet-facing gateway and
// comes back in for one of its hostnames is drawn to the service behind that
// hostname, with the gateway as a badge (like a waypoint). The gateway's own
// arrows stay for what people reach through it: UIs, sign-in pages, and
// anything nothing else calls.
function throughEdge(nodes: LabNode[], edges: LabEdge[]): LabEdge[] {
  const byId = new Map(nodes.map(n => [n.id, n]))
  const gw = nodes.find(n => n.kind === 'gateway' && n.products?.includes('kgateway'))
  if (!gw) return edges
  const routes = edges.filter(e => e.source === gw.id && e.kind !== 'oidc' && e.label !== 'SSO')
  const behind = (host: string) => routes.filter(r => (r.label ?? '').split(', ').includes(host)).map(r => r.target)
  const out: LabEdge[] = []
  const reached = new Set<string>()
  for (const e of edges) {
    if (e.target !== gw.id || e.kind === 'control' || e.kind === 'telemetry') { out.push(e); continue }
    const hosts = [...(e.hosts ?? []), ...[...(e.label ?? '').matchAll(/via edge · ([^\s,]+)/g)].map(m => m[1])]
    const ts = [...new Set(hosts.flatMap(behind))].filter(t => t !== e.source)
    if (!ts.length) { if (hosts.length) continue; out.push(e); continue } // a call to itself through the edge
    for (const t of ts) {
      reached.add(t)
      out.push({ ...e, id: `${e.source}>${t}`, target: t, label: (e.label ?? '').replace(/via edge · \S+/, '').trim() || undefined, kind: e.kind === 'mesh' ? 'http' : e.kind, of: e.of ?? [e.id], via: [...((e as MapEdge).via ?? []), gw.id] } as MapEdge)
    }
  }
  // routes to services that other services call through the edge: shown by the badge on those calls
  return out.filter(e => !(e.source === gw.id && reached.has(e.target) && !['ui', 'idp', 'fabric'].includes(byId.get(e.target)?.kind ?? '')))
}

// throughPools: Substrate routes each call to the worker running the agent,
// so "controller → router → pool" is the controller calling those agents,
// through the router; and what a pool's workers call is what its agents
// call. Both fold onto the agents' own arrows.
function throughPools(nodes: LabNode[], edges: LabEdge[], pools: Record<string, string[]>): LabEdge[] {
  const poolIds = new Set(Object.keys(pools))
  if (!poolIds.size) return edges
  // the router: what calls into the pools, or (between calls, when the mesh
  // has nothing recent) the Substrate workload the agents' caller talks to
  const pooled = new Set(Object.values(pools).flat())
  const routers = new Set(edges.filter(e => poolIds.has(e.target) && !poolIds.has(e.source) && e.kind !== 'substrate').map(e => e.source))
  const subs = new Set(nodes.filter(n => n.products?.includes('substrate') && n.kind !== 'substrate' && n.kind !== 'agent').map(n => n.id))
  for (const e of edges) if (subs.has(e.target) && edges.some(o => o.source === e.source && pooled.has(o.target))) routers.add(e.target)
  const out: LabEdge[] = []
  const agentsOf = (pool: string) => pools[pool] ?? []
  for (const e of edges) {
    // router → pool: the badge on the calls it carries
    if (routers.has(e.source) && poolIds.has(e.target)) continue
    // pool → X: kept only when none of its agents already calls X
    if (poolIds.has(e.source) && e.kind !== 'substrate') {
      if (agentsOf(e.source).some(a => edges.some(o => o.source === a && o.target === e.target))) continue
    }
    out.push({ ...e })
  }
  // caller → router → pool, where the caller has its own arrow to agents there
  for (const r of routers) {
    const fed = edges.filter(e => e.source === r && poolIds.has(e.target))
    const served = fed.length ? new Set(fed.flatMap(e => agentsOf(e.target))) : pooled
    for (const x of out.filter(e => e.target === r)) {
      const carried = out.filter(o => o.source === x.source && served.has(o.target))
      if (!carried.length) continue
      for (const c of carried) Object.assign(c, { via: [...new Set([...((c as MapEdge).via ?? []), r])], of: [...new Set([...((c as MapEdge).of ?? [c.id]), x.id])] })
      out.splice(out.indexOf(x), 1)
    }
  }
  return out
}

function stack(nodes: LabNode[], edges: MapEdge[], expanded: Set<string>, poolOf: Map<string, string>) {
  const ins = new Map<string, string[]>(), outs = new Map<string, string[]>()
  for (const e of edges) { push(ins, e.target, e.source); push(outs, e.source, e.target) }
  const sig = (n: LabNode) => [n.group, n.kind, poolOf.get(n.id) ?? '', (n.products ?? []).join(','), [...new Set(ins.get(n.id) ?? [])].sort().join(','), [...new Set(outs.get(n.id) ?? [])].sort().join(',')].join('|')
  const by = new Map<string, LabNode[]>()
  for (const n of nodes) if (ins.has(n.id) || outs.has(n.id)) push(by, sig(n), n)
  const into = new Map<string, string>()
  const stacks: LabNode[] = []
  for (const [, list] of by) {
    if (list.length < 3 || list[0].kind === 'external' || list[0].kind === 'llm') continue
    const id = `stack:${list.map(n => n.id).sort()[0]}`
    if (expanded.has(id)) continue
    list.sort((a, b) => a.label.localeCompare(b.label))
    const kind = list[0].kind
    stacks.push({ ...list[0], id, label: `${list.length} ${kind === 'agent' ? 'agents' : kind === 'mcp' ? 'MCP servers' : 'workloads'}`,
      sub: list.map(n => n.label).join(', '), ref: undefined, related: list.flatMap(n => (n.ref ? [n.ref] : [])),
      pods: list.flatMap(n => n.pods ?? []), identity: list.flatMap(n => n.identity ?? []), status: worst(list),
      summary: { members: list.map(n => n.id) } })
    for (const n of list) into.set(n.id, id)
  }
  if (!into.size) return { nodes, edges }
  const map = (id: string) => into.get(id) ?? id
  const merged: MapEdge[] = edges.map(e => ({ ...e, id: `${map(e.source)}>${map(e.target)}`, source: map(e.source), target: map(e.target) })).filter(e => e.source !== e.target)
  return { nodes: [...nodes.filter(n => !into.has(n.id)), ...stacks], edges: dedupe(merged) }
}

function dedupe(es: MapEdge[]): MapEdge[] {
  const m = new Map<string, MapEdge>()
  for (const e of es) {
    const id = `${e.source}>${e.target}`
    const x = m.get(id)
    if (x) { x.of = [...new Set([...x.of, ...e.of])]; x.via = [...new Set([...x.via, ...e.via])]; x.observed = x.observed || e.observed; continue }
    m.set(id, { ...e, id, of: [...e.of], via: [...e.via] })
  }
  return [...m.values()]
}

const worst = (list: LabNode[]) => list.map(n => n.status).sort((a, b) => RANK[b] - RANK[a])[0]

function push<K, V>(m: Map<K, V[]>, k: K, v: V) {
  const l = m.get(k)
  if (l) l.push(v); else m.set(k, [v])
}
