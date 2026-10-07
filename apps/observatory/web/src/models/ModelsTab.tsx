import { Background, BackgroundVariant, MarkerType, ReactFlow, ReactFlowProvider, type Edge, type Node } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { cutModel, getModels, type Lab, type LabNode, type ModelProvider, type ModelView, type Traffic } from '../api'
import { edgeTypes, type FlowData } from '../topology/edges'
import { nodeTypes, type CardData } from '../topology/nodes'
import { CAPTION_H, LABEL_H, LABEL_W, TILE } from '../topology/layout'
import { substrateFor } from '../topology/Topology'
import { endpoint, health, kindLabel, serving } from './chain'
import { ModelRules } from './ModelRules'
import { TrafficRow } from '../traffic/TrafficRow'
import '../continuity/continuity.css'
import './models.css'

const POLL_MS = 5000
const pname = (p: ModelProvider) => `${kindLabel(p)} ${p.model}`
const at = (t?: string) => (t ? new Date(t).toLocaleTimeString() : '')
type Move = { id: string; time: string; kind: 'move' | 'cut' | 'restore'; from?: string; to: string; reason: string }

export function ModelsTab({ lab }: { lab: Lab }) {
  const [view, setView] = useState<ModelView>()
  const [err, setErr] = useState<string>()
  const load = useCallback(() => {
    getModels().then(v => { setView(v); setErr(undefined) }, e => setErr((e as Error).message))
  }, [])
  // the chain and its stats change with every call: poll, and reload at once
  // when an outage is cut or restored from anywhere
  useEffect(() => {
    load()
    const t = setInterval(load, POLL_MS)
    return () => clearInterval(t)
  }, [load])
  const { onTraffic } = lab
  useEffect(() => onTraffic(t => { if (t.kind === 'model') load() }), [onTraffic, load])

  if (!view) return <div className="cont-empty subtle">{err ?? 'Reading the model chain…'}</div>
  if (!view.backend) {
    return <div className="cont-empty subtle">No model route found (HTTPRoute llm in agentgateway-system). Run <span className="mono">make llm</span> and it appears here.</div>
  }
  const key = `${view.backend.namespace}/${view.backend.name}`
  return (
    <ReactFlowProvider>
      {/* keyed by backend: unsaved rule edits never carry over to another */}
      <Models key={key} lab={lab} view={view} setView={setView} reload={load} err={err} />
    </ReactFlowProvider>
  )
}

function Models({ lab, view, setView, reload, err }: {
  lab: Lab; view: ModelView; setView: (v: ModelView) => void; reload: () => void; err?: string
}) {
  const be = view.backend!
  const [busy, setBusy] = useState(false)
  const [cutErr, setCutErr] = useState<string>()
  const [choice, setChoice] = useState<string>() // which provider the outage button cuts
  const ps = view.providers
  const live = serving(ps)
  const up = ps.find(p => !p.outage)
  const cuts = ps.filter(p => p.outage)
  const target = ps.find(p => p.name === choice) ?? cuts[0] ?? live ?? ps[0]
  const phase: Phase = !ps.length || !up ? 'down'
    : ps[0].outage ? 'failover'
      : cuts.length ? 'degraded'
        : !live && ps.some(p => health(p).cls === 'bad') ? 'errors' : 'ok'

  const kill = async () => {
    if (!target) return
    setBusy(true); setCutErr(undefined)
    try { await cutModel(be.namespace, be.name, target.name, !target.outage); reload() }
    catch (e) { setCutErr((e as Error).message) } finally { setBusy(false) }
  }

  const nodes = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n]) ?? []), [lab.graph])
  const names = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n.label]) ?? []), [lab.graph])
  const calls = useMemo(() => lab.traffic.filter(t => t.kind === 'llm').slice(0, 120), [lab.traffic])
  const events = useMemo(() => lab.traffic.filter(t => t.kind === 'model').slice(0, 60), [lab.traffic])
  const feed = useMemo(() => lab.traffic.filter(t => t.kind === 'llm' || t.kind === 'model').slice(0, 120), [lab.traffic])
  // Transitions: each time a different provider starts answering (from the
  // calls, oldest first), and each outage simulated or restored
  const moves = useMemo(() => {
    const ps = view?.providers ?? []
    const who = (t: Traffic) => {
      const a = t.attrs ?? {}
      const m = a['gen_ai.response.model'] || ''
      return ps.find(p => !!p.model && (p.model === m || m.startsWith(p.model + '-')))
    }
    const out: Move[] = []
    let last: ModelProvider | undefined
    for (const t of calls.slice().reverse()) {
      const p = t.status === 200 ? who(t) : undefined
      if (!p) continue
      if (last && p.name !== last.name) {
        const down = ps.indexOf(p) > ps.indexOf(last)
        out.push({ id: t.id, time: t.time, kind: 'move', from: last.name, to: p.name,
          reason: down ? `FailoverActivated · ${p.model} answering` : `Failback · ${p.model} answering again` })
      }
      last = p
    }
    for (const t of events) {
      const cut = /cut|outage/i.test(t.summary) && !/restor/i.test(t.summary)
      const name = ps.find(p => t.summary.includes(p.name))
      out.push({ id: t.id, time: t.time, kind: cut ? 'cut' : 'restore', to: name?.name ?? 'provider', reason: `${t.summary}${t.user ? ` · ${t.user}` : ''}` })
    }
    return out.sort((x, y) => Date.parse(y.time) - Date.parse(x.time)).slice(0, 60)
  }, [calls, events, view])

  // Left to right: who may call (the agent pools by mesh identity, and
  // callers outside the mesh with an API key), the AI gateway, and the
  // providers in priority order.
  // an agent pool on Agent Substrate: its workers busy with a session now
  // (they keep running whichever provider answers underneath)
  const pool = useCallback((n: LabNode) => {
    if (n.kind !== 'substrate' || !lab.substrate) return undefined
    const ws = substrateFor(lab.substrate, n).workers
    return ws.length ? `Agent Substrate · ${ws.filter(w => w.actorId).length}/${ws.length} busy` : undefined
  }, [lab.substrate])
  const { rfNodes, rfEdges } = useMemo(() => {
    const rfNodes: Node[] = []
    const rfEdges: Edge[] = []
    const COL = 330, ROW = 170
    const ok = !!up
    // every size is fixed and measured, so wires survive rebuilds (see the
    // Identity Continuity map)
    const tile = (id: string, n: LabNode, col: number, row: number, extra: Partial<CardData> = {}) => {
      const height = TILE + LABEL_H + 4 + (extra.caption ? CAPTION_H : 0)
      rfNodes.push({ id, type: 'tile', position: { x: col * COL, y: row * ROW }, data: { n, ...extra } as CardData,
        width: LABEL_W, height, style: { width: LABEL_W, height }, draggable: false, measured: { width: LABEL_W, height } })
    }
    const wire = (s: string, t: string, data: Partial<FlowData>) => rfEdges.push({ id: `${s}>${t}`, source: s, target: t, type: 'flow',
      sourceHandle: 'o0', targetHandle: 'i0', data: { kind: 'llm', ...data } as FlowData,
      markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14, color: data.state === 'down' ? 'var(--bad)' : data.state === 'active' ? 'var(--ok)' : 'var(--text-subtle)' } })
    // callers: the policy's identities as the graph knows them, else whoever called lately
    const callers: LabNode[] = []
    const seen = new Set<string>()
    const add = (n?: LabNode) => { if (n && !seen.has(n.id)) { seen.add(n.id); callers.push(n) } }
    for (const c of view.callers ?? []) {
      if (c.nodes?.length) c.nodes.forEach(id => add(nodes.get(id)))
      else add({ id: `caller:${c.namespace}/${c.serviceAccount}`, kind: 'workload', label: c.serviceAccount, sub: c.namespace, group: '', status: 'idle', summary: {}, products: [] })
    }
    if (!view.callers) calls.forEach(t => add(t.source ? nodes.get(t.source) : undefined))
    if (view.external) {
      add({ id: 'caller:external', kind: 'external', label: 'external (API key)', sub: view.external.hosts.join(', '), group: '', status: ok ? 'ok' : 'down', summary: {}, products: [] })
    }
    const gw = (view.gateway && nodes.get(view.gateway)) || { id: 'gw:ai-gateway', kind: 'gateway', label: 'ai-gateway', group: '', status: 'ok', summary: {}, products: ['agentgateway'] } as LabNode
    const rows = Math.max(callers.length, ps.length, 1)
    const mid = (rows - 1) / 2
    callers.forEach((n, i) => {
      tile(n.id, n, 0, mid - (callers.length - 1) / 2 + i, { fog: !ok, caption: n.id === 'caller:external' ? n.sub : pool(n) })
      wire(n.id, gw.id, { state: ok ? 'active' : 'down' })
    })
    tile(gw.id, gw, 1, mid, { highlight: ok ? 'ok' : 'bad' })
    ps.forEach((p, i) => {
      const h = health(p)
      const isLive = p.name === live?.name
      const n: LabNode = { id: `model:${p.name}`, kind: 'llm', label: `${i + 1}. ${p.name} · ${kindLabel(p)}`, sub: p.model, group: '',
        status: isLive ? 'ok' : p.outage || h.cls === 'bad' ? 'down' : 'idle', summary: {}, products: [] }
      tile(n.id, n, 2, mid - (ps.length - 1) / 2 + i, { highlight: isLive ? 'ok' : p.outage || h.cls === 'bad' ? 'bad' : undefined,
        outage: !!p.outage, caption: `${p.model} · ${endpoint(p)}` })
      wire(gw.id, n.id, { state: isLive ? 'active' : p.outage ? 'down' : 'standby', rps: isLive ? 1 : 0,
        cut: !!p.outage, label: p.outage ? 'outage (simulated)' : undefined })
    })
    return { rfNodes, rfEdges }
  }, [view, ps, up, live, nodes, calls, pool])

  return (
    <div className="cont">
      <Banner phase={phase} live={live ?? up} primary={ps[0]} cuts={cuts} />
      <div className="cont-main">
        <div className="cont-canvas">
          <div className="cont-bar">
            <span className="subtle small mono">{be.namespace}/{be.name}</span>
            <span className="grow" />
            {target && (
              <div className="outage">
                <div className="row">
                  {ps.length > 1 && (
                    <select className="field" value={target.name} onChange={e => setChoice(e.target.value)} title="the model provider to cut off" aria-label="Model provider to cut off">
                      {ps.map(p => <option key={p.name} value={p.name}>{p.name}{p.outage ? ' (cut)' : ''}</option>)}
                    </select>
                  )}
                  <button className={target.outage ? 'btn big ok' : 'btn big danger'} disabled={busy} onClick={kill}
                    title={target.outage ? `Point ${target.name} back at ${endpoint(target)}` : `Point ${target.name} at a closed port, so its calls fail as in a real outage`}>
                    {target.outage ? 'Restore model provider' : 'Simulate model outage'}
                  </button>
                </div>
                <span className="subtle small">{target.outage ? `${target.name} cut ${at(target.outage.since)}${target.outage.by ? ` by ${target.outage.by}` : ''} · was ${endpoint(target)}`
                  : `cuts ${target.name} (${endpoint(target)}) at the gateway`}</span>
              </div>
            )}
          </div>
          {(cutErr || err) && <div className="note bad cont-err">{cutErr ?? err}</div>}
          <ReactFlow nodes={rfNodes} edges={rfEdges} nodeTypes={nodeTypes} edgeTypes={edgeTypes} fitView fitViewOptions={{ padding: 0.15 }}
            nodesConnectable={false} proOptions={{ hideAttribution: true }} minZoom={0.2}>
            <Background variant={BackgroundVariant.Dots} gap={22} size={1} color="var(--border)" />
          </ReactFlow>
          <ModelHealth providers={ps} />
        </div>
        <ModelRules view={view} onSaved={setView} />
      </div>
      <div className="cont-foot">
        <div className="cont-col">
          <div className="label">Transitions</div>
          <div className="scroll trans">
            {moves.map(m => (
              <div key={m.id} className="row line">
                <span className="mono subtle">{at(m.time)}</span>
                {m.kind === 'move' ? <>
                  <span className="chip">{m.from}</span>→<span className="chip accent">{m.to}</span>
                  <span className="subtle ellipsis grow">{m.reason}</span>
                </> : <>
                  <span className={`chip ${m.kind === 'cut' ? 'bad' : 'ok'}`}>{m.kind === 'cut' ? 'outage' : 'restored'}</span>
                  <span className="chip">{m.to}</span>
                  <span className="subtle ellipsis grow">{m.reason}</span>
                </>}
              </div>
            ))}
            {!moves.length && <div className="subtle small">No failovers yet. Simulate an outage and watch the calls move.</div>}
          </div>
        </div>
        <div className="cont-col grow">
          <div className="label">Model traffic</div>
          <div className="scroll trans">
            {feed.map(t => <TrafficRow key={t.id} t={t} names={names} compact />)}
            {!feed.length && <div className="subtle small">Ask any agent something and its model calls stream in here.</div>}
          </div>
        </div>
      </div>
    </div>
  )
}

type Phase = 'ok' | 'failover' | 'degraded' | 'errors' | 'down'

// Banner: the one line a room reads from the back: which model is answering,
// and, during an outage, what happened and what the gateway did.
function Banner({ phase, live, primary, cuts }: { phase: Phase; live?: ModelProvider; primary?: ModelProvider; cuts: ModelProvider[] }) {
  const upper = (p?: ModelProvider) => (p ? pname(p).toUpperCase() : '')
  const cls = phase === 'ok' ? 'ok' : phase === 'degraded' ? 'warn' : 'bad'
  const head = {
    ok: `CONNECTED · ${upper(live)} SERVING MODEL CALLS`,
    failover: `MODEL FAILOVER ACTIVE · ${upper(live)} SERVING`,
    degraded: `${upper(live)} SERVING · A FALLBACK IS OUT`,
    errors: 'MODEL ERRORS · THE PROVIDERS ARE FAILING CALLS',
    down: 'MODEL CALLS UNAVAILABLE · NO PROVIDER IN ROTATION',
  }[phase]
  const why = cuts.map(p => `${pname(p)} outage simulated${p.outage?.by ? ` by ${p.outage.by}` : ''}${p.outage?.since ? ` at ${at(p.outage.since)}` : ''}`).join(' · ')
  return (
    <div className={`banner ${cls} p-${phase === 'failover' || phase === 'down' ? 'failover' : phase}`}>
      <span className={`dot ${cls}`} />
      <span className="bhead">{head}</span>
      {why && <span className="why">{why}</span>}
      {phase === 'failover' && primary && !why && <span className="why">{pname(primary)} out of rotation</span>}
    </div>
  )
}

// ModelHealth: each provider's recent calls, in priority order.
function ModelHealth({ providers }: { providers: ModelProvider[] }) {
  return (
    <div className="health">
      {providers.map(p => {
        const h = health(p)
        return (
          <div key={p.name} className="hrow">
            <span className={`dot ${h.cls}`} />
            <b title={pname(p)}>{p.name}</b>
            <span className={`chip ${h.cls === 'idle' ? '' : h.cls}`}>{h.text}</span>
          </div>
        )
      })}
    </div>
  )
}
