import {
  Background, BackgroundVariant, ControlButton, Controls, MarkerType, MiniMap, ReactFlow, ReactFlowProvider, getNodesBounds, getViewportForBounds, useReactFlow,
  type Edge, type Node,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { toPng } from 'html-to-image'
import { useEffect, useMemo, useRef, useState } from 'react'
import type { EdgeKind, Lab, LabNode, Pod, Substrate, Traffic } from '../api'
import { DetailsPanel } from './DetailsPanel'
import { edgeTypes, type FlowData } from './edges'
import { PRODUCTS, ProductIcon } from './icons'
import { LABEL_H, LABEL_W, OUTSIDE, layout, type Placed } from './layout'
import { LENSES, mapView, type Lens, type Tray } from './mapview'
import { nodeTypes, type CardData, type GroupData } from './nodes'
import './topology.css'

const LEGEND: EdgeKind[] = ['http', 'mcp', 'a2a', 'llm', 'oidc', 'db', 'substrate']

export function Topology(props: { lab: Lab; focus?: string; onFocused: () => void }) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  )
}

// One map of the whole lab. Parties are boxes in call order; arrows are
// what calls what, thicker with traffic, red when denied, grey and dashed
// when nothing has flowed lately. Hover a node to trace its neighbours.
function Canvas({ lab, focus, onFocused }: { lab: Lab; focus?: string; onFocused: () => void }) {
  const rf = useReactFlow()
  const [selected, setSelected] = useState<string>()
  const [hoverNode, setHoverNode] = useState<string>()
  const [hoverWire, setHoverWire] = useState<{ id: string; x: number; y: number }>()
  const [query, setQuery] = useState('')
  const [pinned, setPinned] = useState<string>()
  const [hovered, setHovered] = useState<string>()
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [lens, setLens] = useState<Lens>(() => { try { return (localStorage.getItem('obs.lens') as Lens) || 'all' } catch { return 'all' } })
  const product = hovered ?? pinned
  const [placed, setPlaced] = useState<Placed>()
  const pulses = usePulses(lab)
  const g = lab.graph

  const view = useMemo(() => (g ? mapView(g.nodes, g.edges, expanded, lens) : { nodes: [] as LabNode[], edges: [], trays: {} as Record<string, Tray>, folded: {} as Record<string, LabNode> }), [g, expanded, lens])
  useEffect(() => { try { localStorage.setItem('obs.lens', lens) } catch { /* private mode */ } }, [lens])

  // re-layout only when the shape changes, so live updates don't reshuffle
  const shape = useMemo(() => view.nodes.map(n => n.id).join('|') + '#' + view.edges.map(e => e.id).join('|') + '#' + Object.entries(view.trays).map(([k, t]) => k + t.members.join(',')).join('|'), [view])
  const firstLayout = useRef(true)
  useEffect(() => {
    if (!g) return
    let stale = false
    layout(g, view.nodes, view.edges, view.trays).then(p => { if (!stale) setPlaced(p) }).catch(err => console.error('layout', err))
    return () => { stale = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shape, g?.groups])
  useEffect(() => { firstLayout.current = true }, [lens])
  useEffect(() => {
    if (!placed || !firstLayout.current) return
    firstLayout.current = false
    requestAnimationFrame(() => requestAnimationFrame(() => rf.fitView({ padding: { top: '96px', left: '24px', bottom: '24px', right: '250px' } })))
  }, [placed, rf])

  // where a product from the rail is on the map: tiles, trays, badges on
  // wires (waypoints, the edge, the Substrate router), and, for instances
  // folded out of view, a count on their zone's status bar
  const productHits = useMemo(() => {
    if (!product || !g || !placed) return undefined
    const byId = new Map(g.nodes.map(n => [n.id, n]))
    const is = (id: string) => !!byId.get(id)?.products?.includes(product)
    const tiles = new Set(view.nodes.filter(n => n.products?.includes(product)).map(n => n.id))
    const trays = new Set(Object.entries(view.trays).filter(([, t]) => t.node.products?.includes(product)).map(([id]) => id))
    const wires = new Set(placed.wires.filter(w => w.via.some(is)).map(w => w.id))
    const drawn = new Set<string>([...tiles, ...trays, ...placed.wires.flatMap(w => w.via)])
    for (const n of view.nodes) for (const m of (n.summary?.members as string[] | undefined) ?? []) drawn.add(m)
    const zones: Record<string, number> = {}
    for (const n of g.nodes) {
      if (!n.products?.includes(product) || drawn.has(n.id) || n.group === 'party:cluster') continue
      zones[n.group] = (zones[n.group] ?? 0) + 1
    }
    return { tiles, trays, wires, zones }
  }, [product, g, placed, view])

  // what's in focus: a hovered or selected node and its neighbours, or
  // every instance of a product from the rail
  const focusIds = useMemo(() => {
    const at = hoverNode ?? selected
    if (at) {
      const s = new Set([at])
      for (const e of view.edges) { if (e.source === at) s.add(e.target); if (e.target === at) s.add(e.source) }
      // a pool's tray lights with its agents
      for (const [pool, t] of Object.entries(view.trays)) {
        if (pool === at) t.members.forEach(m => s.add(m))
        if (t.members.includes(at)) s.add(pool)
      }
      return s
    }
    if (productHits && placed) {
      const s = new Set([...productHits.tiles, ...productHits.trays])
      for (const w of placed.wires) if (productHits.wires.has(w.id)) { s.add(w.source); s.add(w.target) }
      return s
    }
    return undefined
  }, [hoverNode, selected, productHits, placed, view])

  const rateOf = useMemo(() => (of: string[]) => {
    const r = { rps: 0, l4: 0, errors: 0 }
    for (const id of of) { const st = lab.stats?.edges[id]; r.rps += st?.rps ?? 0; r.l4 += st?.l4 ?? 0; r.errors += st?.errors ?? 0 }
    return r
  }, [lab.stats])
  const perNode = useMemo(() => {
    const out: Record<string, { rps: number; errors: number }> = {}
    for (const e of view.edges) { const r = (out[e.target] ??= { rps: 0, errors: 0 }); const x = rateOf(e.of); r.rps += x.rps; r.errors += x.errors }
    return out
  }, [view.edges, rateOf])
  const p95 = useP95(lab.traffic)
  const sub = lab.substrate

  // each zone's live status: everything running in it, not only what's
  // drawn. The status bar and the zone's details panel read the same record.
  const zones = useMemo(() => {
    const out: Record<string, LabNode> = {}
    if (!g) return out
    const into = new Map<string, number>()
    for (const e of g.edges) into.set(e.target, (into.get(e.target) ?? 0) + 1)
    for (const grp of g.groups) {
      const members = g.nodes.filter(n => n.group === grp.id && n.kind !== 'cluster')
      const ids = new Set(members.map(m => m.id))
      const seen = new Set<string>(), pods: Pod[] = []
      for (const m of members) for (const p of m.pods ?? []) {
        const k = `${m.namespace ?? ''}/${p.name}`
        if (!seen.has(k)) { seen.add(k); pods.push(p) }
      }
      let rps = 0, errs = 0
      for (const e of g.edges) {
        if (!ids.has(e.target)) continue
        const st = lab.stats?.edges[e.id]
        rps += st?.rps ?? 0; errs += st?.errors ?? 0
      }
      const idle = (view.folded[grp.id]?.summary?.members as string[] | undefined) ?? []
      const agents = members.filter(n => n.kind === 'agent').length
      const stats = { agents, workloads: members.length - agents, ready: pods.filter(p => p.ready).length, pods: pods.length, rps, err: rps > 0 ? errs / rps : 0,
        idle: idle.length, down: members.filter(n => n.status === 'down' || n.status === 'warn').length }
      out[grp.id] = { id: `zone:${grp.id}`, kind: 'cluster', group: grp.id, label: grp.label, sub: grp.domain, status: worstOf(members), pods,
        products: [...new Set(members.flatMap(n => n.products ?? []))], related: members.flatMap(n => (n.ref ? [n.ref] : [])),
        summary: { zone: true, members: members.map(m => m.id), idle, stats } }
    }
    return out
  }, [g, lab.stats, view.folded])

  const rfNodes: Node[] = useMemo(() => {
    if (!g || !placed) return []
    const out: Node[] = []
    const q = query.trim().toLowerCase()
    for (const [gid, box] of Object.entries(placed.groups)) {
      const grp = g.groups.find(x => x.id === gid)
      const outside = gid === OUTSIDE
      const z = zones[gid]
      const stats = outside ? undefined : (z?.summary?.stats as GroupData['stats'])
      out.push({ id: gid, type: 'party', position: { x: box.x, y: box.y },
        data: { label: outside ? 'Outside the lab' : grp?.label ?? gid, domain: outside ? undefined : grp?.domain, stats, outside,
          hits: productHits?.zones[gid], product,
          onOpen: outside ? undefined : () => setSelected(`zone:${gid}`) },
        width: box.w, height: box.h, style: { width: box.w, height: box.h }, measured: { width: box.w, height: box.h }, selectable: false, draggable: false, zIndex: -2 })
    }
    for (const [pid, box] of Object.entries(placed.trays)) {
      const t = view.trays[pid]
      if (!t) continue
      const data: CardData = { n: t.node, selected: selected === pid, fog: !!focusIds && !focusIds.has(pid), lit: !!productHits?.trays.has(pid), ...(sub ? substrateFor(sub, t.node) : {}) }
      out.push({ id: pid, type: 'tray', position: { x: box.x, y: box.y }, data, width: box.w, height: box.h, style: { width: box.w, height: box.h }, measured: { width: box.w, height: box.h },
        draggable: false, zIndex: -1 })
    }
    placed.columns.forEach((c, i) => {
      out.push({ id: `col:${i}`, type: 'colhead', position: { x: c.x, y: placed.top }, data: { label: c.label, step: i + 1 }, width: c.w, height: 40, measured: { width: c.w, height: 40 },
        selectable: false, draggable: false })
    })
    for (const n of view.nodes) {
      const box = placed.nodes[n.id]
      if (!box) continue
      const r = perNode[n.id]
      const data: CardData = { n, rps: r?.rps, err: r && r.rps > 0 ? r.errors / r.rps : 0, p95: p95[n.id], selected: selected === n.id,
        lit: !hoverNode && !selected && !!productHits?.tiles.has(n.id),
        fog: (q !== '' && !`${n.label} ${n.namespace ?? ''} ${n.kind}`.toLowerCase().includes(q)) || (!!focusIds && !focusIds.has(n.id)) }
      if (n.kind === 'substrate' && sub) Object.assign(data, substrateFor(sub, n))
      if (n.kind === 'agent' && n.badges?.includes('substrate') && sub) data.actor = actorFor(sub, n)
      if (n.id === placed.door) {
        out.push({ id: n.id, type: 'door', position: { x: box.x, y: box.y }, data, width: box.w, height: box.h, style: { width: box.w, height: box.h }, measured: { width: box.w, height: box.h }, draggable: false })
        continue
      }
      out.push({ id: n.id, type: 'tile', position: { x: box.x - (LABEL_W - box.w) / 2, y: box.y }, data,
        width: LABEL_W, height: box.h + LABEL_H + 4, style: { width: LABEL_W, height: box.h + LABEL_H + 4 }, draggable: false,
        measured: { width: LABEL_W, height: box.h + LABEL_H + 4 } })
    }
    return out
  }, [g, placed, view.nodes, zones, perNode, p95, selected, sub, query, focusIds, hoverNode, product, productHits])

  const names = useMemo(() => new Map(g?.nodes.map(n => [n.id, n.label]) ?? []), [g])
  const viaLabel = useMemo(() => (id: string) => {
    const n = g?.nodes.find(x => x.id === id)
    if (!n) return id
    if (n.kind === 'waypoint') return n.products?.includes('agentgateway') ? 'agentgateway waypoint' : 'Istio waypoint'
    return n.products?.includes('kgateway') ? `kgateway ${n.label}` : n.label
  }, [g])
  const rfEdges: Edge[] = useMemo(() => {
    if (!placed) return []
    // one badge per hop: a waypoint in front of the callee sits at the
    // callee's side, anything else (the edge, a router) at the caller's
    const groupOf = new Map(g?.nodes.map(n => [n.id, n.group]) ?? [])
    const badged = new Set<string>()
    const badgeFor = (w: Placed['wires'][number]): 'src' | 'dst' | undefined => {
      if (!w.via.length) return undefined
      const at = w.via.some(v => groupOf.get(v) === groupOf.get(w.target)) ? 'dst' : 'src'
      const key = `${at === 'dst' ? w.target : w.source}|${w.via.join(',')}`
      if (badged.has(key)) return undefined
      badged.add(key)
      return at
    }
    return placed.wires.map(w => {
      const r = rateOf(w.of)
      const ps = w.of.flatMap(id => (pulses[id] ?? []).map(p => ({ ...p, id: p.id + w.id }))).slice(-6)
      const inFocus = productHits
        ? productHits.wires.has(w.id) || productHits.tiles.has(w.source) || productHits.tiles.has(w.target)
        : !focusIds || w.ends.some((id, i) => i % 2 === 0 && focusIds.has(id) && focusIds.has(w.ends[i + 1]))
      const data: FlowData = { kind: w.kind, rps: r.rps, l4: r.l4, errors: r.errors, pulses: ps, points: w.points,
        via: w.via.map(viaLabel), badgeAt: badgeFor(w), fog: !inFocus, focused: !!focusIds && inFocus, badgeLit: !!productHits?.wires.has(w.id) }
      const color = r.errors > 0 ? 'var(--bad)' : `var(--k-${w.kind})`
      return { id: w.id, source: w.source, target: w.target, sourceHandle: 'o0', targetHandle: 'i0', type: 'flow', data,
        markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14, color }, zIndex: inFocus && focusIds ? 5 : 1 }
    })
  }, [placed, rateOf, pulses, focusIds, viaLabel, g, productHits])

  useEffect(() => {
    if (!focus || !placed) return
    setSelected(focus)
    const n = rf.getNode(focus)
    if (n) rf.fitView({ nodes: [n], padding: 2, duration: 500, maxZoom: 1.2 })
    onFocused()
  }, [focus, placed, rf, onFocused])

  const node = view.nodes.find(n => n.id === selected) ?? Object.values(zones).find(n => n.id === selected) ?? Object.values(view.folded).find(n => n.id === selected) ?? g?.nodes.find(n => n.id === selected)

  return (
    <div className="topo">
      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodeClick={(_, n) => n.type !== 'party' && n.type !== 'colhead' && setSelected(n.id)}
        onNodeMouseEnter={(_, n) => (n.type === 'tile' || n.type === 'tray' || n.type === 'door') && setHoverNode(n.id)}
        onNodeMouseLeave={() => setHoverNode(undefined)}
        onPaneClick={() => setSelected(undefined)}
        onEdgeMouseEnter={(ev, e) => setHoverWire({ id: e.id, x: ev.clientX, y: ev.clientY })}
        onEdgeMouseMove={(ev, e) => setHoverWire({ id: e.id, x: ev.clientX, y: ev.clientY })}
        onEdgeMouseLeave={() => setHoverWire(undefined)}
        minZoom={0.1}
        maxZoom={2}
        proOptions={{ hideAttribution: true }}
        nodesConnectable={false}
      >
        <Background variant={BackgroundVariant.Dots} gap={22} size={1} color="var(--border)" />
        <MiniMap pannable zoomable className="minimap" nodeColor={n => (n.type === 'tile' || n.type === 'door' ? 'var(--accent)' : 'transparent')} maskColor="var(--fog)" />
        <Controls showInteractive={false}>
          <ControlButton onClick={() => exportMap(rf.getNodes(), lens)} title="Export the whole map as a PNG" aria-label="Export map">
            <svg viewBox="0 0 24 24" fill="currentColor"><path d="M11 3h2v9.2l3.3-3.3 1.4 1.4L12 16l-5.7-5.7 1.4-1.4 3.3 3.3V3zM4 18h16v2H4z" /></svg>
          </ControlButton>
        </Controls>
      </ReactFlow>

      <div className="toolbar">
        <div className="seg">
          {LENSES.map(l => <button key={l.id} className={lens === l.id ? 'on' : ''} title={l.hint} onClick={() => setLens(l.id)}>{l.label}</button>)}
        </div>
        <input className="field search" placeholder="Find a workload…" value={query} onChange={e => setQuery(e.target.value)} />
      </div>
      <div className="rail">
        <div className="label">Solo products</div>
        {PRODUCTS.map(p => {
          const count = g?.nodes.filter(n => n.products?.includes(p.id) && n.group !== 'party:cluster').length ?? 0
          if (!count) return null
          return (
            <button key={p.id} className={pinned === p.id ? 'prod on' : 'prod'} title={p.blurb}
              onMouseEnter={() => setHovered(p.id)} onMouseLeave={() => setHovered(undefined)}
              onClick={() => setPinned(pinned === p.id ? undefined : p.id)}>
              <ProductIcon id={p.id} size={16} />
              <span className="grow">{p.label}</span>
              <span className="subtle mono">{count}</span>
            </button>
          )
        })}
        {product && (() => {
          const p = PRODUCTS.find(x => x.id === product)
          return p && <div className="blurb subtle">{p.blurb} · <a href={p.docs} target="_blank" rel="noopener noreferrer">docs ↗</a></div>
        })()}
        <a className="rail-foot subtle" href="https://docs.solo.io" target="_blank" rel="noopener noreferrer">docs.solo.io ↗</a>
      </div>
      <div className="legend">
        {LEGEND.map(k => <span key={k}><i style={{ background: `var(--k-${k})` }} />{k}</span>)}
        <span><i className="idle" />idle</span>
        <span><i style={{ background: 'var(--bad)' }} />denied</span>
      </div>
      {hoverWire && placed && <WireInfo lab={lab} wire={placed.wires.find(w => w.id === hoverWire.id)} at={hoverWire} names={names} rate={rateOf} />}
      {!g && <div className="empty">Waiting for the lab…</div>}
      {node && <DetailsPanel key={node.id} lab={lab} node={node} onClose={() => setSelected(undefined)} onOpenNode={id => {
        const st = view.nodes.find(n => (n.summary?.members as string[] | undefined)?.includes(id))
        if (st?.id.startsWith('stack:')) setExpanded(s => new Set(s).add(st.id))
        setSelected(id)
      }} onExpand={id => { if (id.startsWith('stack:')) setExpanded(s => new Set(s).add(id)); setSelected(undefined) }} />}
    </div>
  )
}

// WireInfo: what a wire stands for, and the calls it carried recently.
function WireInfo({ lab, wire, at, names, rate }: { lab: Lab; wire?: Placed['wires'][number]; at: { x: number; y: number }; names: Map<string, string>; rate: (of: string[]) => { rps: number; l4: number; errors: number } }) {
  if (!wire) return null
  const r = rate(wire.of)
  const hops = new Set(wire.of)
  const pairs: string[] = []
  for (let i = 0; i < wire.ends.length; i += 2) {
    const p = `${names.get(wire.ends[i]) ?? wire.ends[i]} → ${names.get(wire.ends[i + 1]) ?? wire.ends[i + 1]}`
    if (!pairs.includes(p)) pairs.push(p)
  }
  const recent = lab.traffic.filter(t => {
    const ids = t.via && t.source && t.target ? [`${t.source}>${t.via}`, `${t.via}>${t.target}`] : t.source && t.target ? [`${t.source}>${t.target}`] : t.via && t.target ? [`${t.via}>${t.target}`] : []
    return ids.some(id => hops.has(id))
  })
  const by = new Map<string, { n: number; last: string; outcome: string }>()
  for (const t of recent.slice(0, 300)) {
    const k = t.summary
    const x = by.get(k)
    if (x) x.n++; else by.set(k, { n: 1, last: t.time, outcome: t.outcome })
  }
  const top = [...by].sort((a, b) => b[1].n - a[1].n).slice(0, 6)
  return (
    <div className="wireinfo" style={{ left: at.x + 14, top: at.y + 14 }}>
      <div className="label">{wire.kind} · {pairs.length} connection{pairs.length === 1 ? '' : 's'}</div>
      {pairs.slice(0, 5).map(p => <div key={p} className="mono small">{p}</div>)}
      {pairs.length > 5 && <div className="subtle small">+{pairs.length - 5} more</div>}
      <div className="row small" style={{ marginTop: 6 }}>
        <span className="chip">{r.rps.toFixed(2)} req/s</span>
        <span className="chip" title="new mesh connections per second, from ztunnel">{(r.l4 * 60).toFixed(1)} conn/min</span>
        {r.errors > 0 && <span className="chip bad">{r.errors.toFixed(2)} denied/s</span>}
      </div>
      <div className="label" style={{ marginTop: 8 }}>Recent calls</div>
      {top.length === 0 && <div className="subtle small">{r.l4 > 0 ? 'Mesh connections, but no gateway saw a request: a direct connection (a database pool, a JWKS fetch, a health check).' : 'Nothing lately: configured, idle.'}</div>}
      {top.map(([k, v]) => (
        <div key={k} className="row small"><span className={`dot ${v.outcome === 'ok' ? 'ok' : v.outcome === 'info' ? 'idle' : 'bad'}`} /><span className="grow ellipsis mono">{k}</span><span className="subtle mono">×{v.n}</span></div>
      ))}
    </div>
  )
}

// usePulses turns each live request into a short-lived pulse on every edge
// it crossed (caller → gateway → target).
function usePulses(lab: Lab) {
  const [pulses, setPulses] = useState<Record<string, { id: string; bad: boolean; at: number }[]>>({})
  const buf = useRef<Record<string, { id: string; bad: boolean; at: number }[]>>({})
  useEffect(() => lab.onTraffic((t: Traffic) => {
    const hops = t.via && t.source && t.target ? [`${t.source}>${t.via}`, `${t.via}>${t.target}`]
      : t.source && t.target ? [`${t.source}>${t.target}`] : t.via && t.target ? [`${t.via}>${t.target}`] : []
    const bad = t.outcome === 'error' || t.outcome === 'denied'
    for (const h of hops) (buf.current[h] ??= []).push({ id: t.id + h, bad, at: Date.now() })
  }), [lab])
  useEffect(() => {
    const iv = setInterval(() => {
      const now = Date.now()
      const next: typeof buf.current = {}
      let changed = false
      for (const [k, v] of Object.entries(buf.current)) {
        const keep = v.filter(p => now - p.at < 1200).slice(-6)
        if (keep.length) next[k] = keep
        if (keep.length !== v.length) changed = true
      }
      buf.current = next
      if (changed || Object.keys(next).length) setPulses({ ...next })
    }, 200)
    return () => clearInterval(iv)
  }, [])
  return pulses
}

function useP95(traffic: Traffic[]) {
  return useMemo(() => {
    const cut = Date.now() - 120_000
    const by: Record<string, number[]> = {}
    for (const t of traffic) {
      if (!t.durationMs || Date.parse(t.time) < cut) continue
      for (const id of [t.via, t.target]) if (id) (by[id] ??= []).push(t.durationMs)
    }
    const out: Record<string, number> = {}
    for (const [id, v] of Object.entries(by)) {
      v.sort((a, b) => a - b)
      out[id] = v[Math.min(v.length - 1, Math.floor(v.length * 0.95))]
    }
    return out
  }, [traffic])
}

export function substrateFor(sub: Substrate, n: LabNode) {
  return {
    workers: sub.workers.filter(w => w.workerPool === n.label && w.workerNamespace === n.namespace),
    actors: sub.actors.filter(a => a.workerPoolName === n.label || (!a.workerPoolName && a.atespace === n.namespace)),
  }
}

export function actorFor(sub: Substrate, n: LabNode) {
  const tpls = new Set(sub.actorTemplates.filter(t => t.namespace === n.namespace && (t.harnessName === n.label || t.name.startsWith(n.label + '-'))).map(t => t.name))
  const mine = sub.actors.filter(a => a.actorTemplateNamespace === n.namespace && tpls.has(a.actorTemplateName))
  return mine.find(a => a.status === 'Running') ?? mine.find(a => a.status === 'Resuming') ?? mine[0]
}

const RANK: Record<string, number> = { down: 3, warn: 2, idle: 1, ok: 0 }
const worstOf = (list: LabNode[]): LabNode['status'] => list.map(n => n.status).sort((a, b) => RANK[b] - RANK[a])[0] ?? 'ok'

// exportMap renders the whole map (every lane, tile, wire and badge, not
// just what's in view) to a PNG, on the page's own background.
async function exportMap(nodes: Node[], lens: string) {
  const el = document.querySelector<HTMLElement>('.topo .react-flow__viewport')
  if (!el || !nodes.length) return
  const PAD = 60
  const b = getNodesBounds(nodes)
  const w = Math.ceil(b.width + 2 * PAD), h = Math.ceil(b.height + 2 * PAD)
  const vp = getViewportForBounds(b, w, h, 1, 1, `${PAD}px`)
  // crisp, but inside every browser's canvas limit (Safari's is ~16M pixels)
  const ratio = Math.max(0.5, Math.min(2, Math.sqrt(16e6 / (w * h))))
  const bg = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim() || '#12012a'
  const url = await toPng(el, {
    backgroundColor: bg, width: w, height: h, pixelRatio: ratio,
    style: { width: `${w}px`, height: `${h}px`, transform: `translate(${vp.x}px, ${vp.y}px) scale(${vp.zoom})` },
  })
  const a = document.createElement('a')
  a.href = url
  a.download = `observatory-topology-${lens}-${new Date().toISOString().slice(0, 16).replace(/[:T]/g, '-')}.png`
  a.click()
}
