import { Background, BackgroundVariant, MarkerType, ReactFlow, ReactFlowProvider, type Edge, type Node } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { useMemo, useState } from 'react'
import { api, type IdentityContinuity, type Lab, type LabNode, type TierStatus } from '../api'
import { edgeTypes, type FlowData } from '../topology/edges'
import { nodeTypes, type CardData } from '../topology/nodes'
import { LABEL_H, LABEL_W, TILE } from '../topology/layout'
import { TrafficRow } from '../traffic/TrafficRow'
import { RuleBuilder } from './RuleBuilder'
import './continuity.css'


export function ContinuityTab({ lab }: { lab: Lab }) {
  const items = lab.continuity?.items ?? []
  const [pick, setPick] = useState(0)
  const ic = items[Math.min(pick, items.length - 1)]
  if (!ic) {
    return <div className="cont-empty subtle">No IdentityContinuity resources found. Create one and it appears here.</div>
  }
  return (
    <ReactFlowProvider>
      <Continuity lab={lab} ic={ic} items={items} pick={pick} setPick={setPick} />
    </ReactFlowProvider>
  )
}

function Continuity({ lab, ic, items, pick, setPick }: {
  lab: Lab; ic: IdentityContinuity; items: IdentityContinuity[]; pick: number; setPick: (i: number) => void
}) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string>()
  const tiers = ic.spec.tiers ?? []
  const status = new Map((ic.status?.tiers ?? []).map(t => [t.name, t]))
  const active = ic.status?.active
  const primary = tiers.find(t => t.enabled !== false && !t.drain && status.get(t.name)?.configured !== false)
  const partitioned = new Set((lab.continuity?.partitions ?? []).map(p => p.tier))
  const target = tiers.find(t => t.type === 'oidc' && t.name === active) ?? tiers.find(t => t.type === 'oidc' && partitioned.has(t.name))
    ?? tiers.find(t => t.type === 'oidc' && status.get(t.name)?.configured !== false)
  const cut = target ? partitioned.has(target.name) : false
  const failover = !!active && !!primary && active !== primary.name
  const nodes = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n]) ?? []), [lab.graph])
  const key = `${ic.metadata.namespace}/${ic.metadata.name}`
  const paths = (lab.continuity?.paths ?? []).filter(p => p.instance === key)
  const brokerNode = paths[0] ? nodes.get(paths[0].broker) : undefined

  const kill = async () => {
    if (!target) return
    setBusy(true); setErr(undefined)
    try { await api('/api/continuity/partition', { method: 'POST', body: JSON.stringify({ tier: target.name, down: !cut }) }) }
    catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }

  // Centred on the identity provider, left to right: every app that signs
  // people in through this broker, the broker (the issuer everything
  // trusts), and the upstream tiers it authenticates against, in failover
  // order. When no tier is healthy, every app on the left goes dark.
  const { rfNodes, rfEdges } = useMemo(() => {
    const rfNodes: Node[] = []
    const rfEdges: Edge[] = []
    const COL = 320, ROW = 160
    const signInOK = !!active
    const tile = (id: string, n: LabNode | undefined, col: number, row: number, extra: Partial<CardData> = {}) => {
      if (!n) return
      rfNodes.push({ id, type: 'tile', position: { x: col * COL, y: row * ROW }, data: { n, ...extra } as CardData,
        width: LABEL_W, height: TILE + LABEL_H + 4, style: { width: LABEL_W, height: TILE + LABEL_H + 4 }, draggable: false })
    }
    const wire = (s: string, t: string, data: Partial<FlowData>) => rfEdges.push({ id: `${s}>${t}`, source: s, target: t, type: 'flow',
      sourceHandle: 'o0', targetHandle: 'i0', data: { kind: 'oidc', ...data } as FlowData,
      markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14, color: data.state === 'down' ? 'var(--bad)' : data.state === 'active' ? 'var(--ok)' : 'var(--text-subtle)' } })
    const broker = paths[0]?.broker
    const apps = [...new Set(paths.map(p => p.app))]
    const rows = Math.max(apps.length, tiers.length, 1)
    const mid = (rows - 1) / 2
    apps.forEach((id, i) => {
      tile(id, nodes.get(id), 0, mid - (apps.length - 1) / 2 + i, { fog: !signInOK })
      if (broker) wire(id, broker, { state: signInOK ? 'active' : 'down' })
    })
    if (broker) tile(broker, nodes.get(broker), 1, mid, { highlight: signInOK ? 'ok' : 'bad' })
    tiers.forEach((t, i) => {
      const st = status.get(t.name)
      const isActive = t.name === active
      const down = partitioned.has(t.name) || (!!st && st.configured !== false && !st.healthy)
      const n: LabNode = { id: `tier:${t.name}`, kind: t.type === 'local' ? 'idp' : 'external', label: `${i + 1}. ${t.displayName ?? t.name}`, group: '',
        status: isActive ? 'ok' : down ? 'down' : 'idle', sub: t.type === 'local' ? 'local accounts' : t.oidc?.issuer, summary: {}, products: [] }
      tile(n.id, n, 2, mid - (tiers.length - 1) / 2 + i, { fog: !isActive && (down || st?.configured === false || t.enabled === false || !!t.drain), highlight: isActive ? 'ok' : down ? 'bad' : undefined })
      if (broker) wire(broker, n.id, { state: isActive ? 'active' : down ? 'down' : 'standby', rps: isActive ? 1 : 0 })
    })
    return { rfNodes, rfEdges }
  }, [tiers, status, active, partitioned, nodes, paths])

  const names = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n.label]) ?? []), [lab.graph])
  const feed = useMemo(() => lab.traffic.filter(t => t.kind === 'continuity' || t.kind === 'oidc').slice(0, 120), [lab.traffic])
  const activeName = tiers.find(t => t.name === active)?.displayName ?? active

  return (
    <div className="cont">
      <div className={`banner ${!active ? 'bad' : failover ? 'bad' : 'ok'}`}>
        <span className={`dot ${!active ? 'bad' : failover ? 'bad' : 'ok'}`} />
        {!active ? 'SIGN-IN UNAVAILABLE · NO HEALTHY TIER'
          : failover ? `FAILOVER ACTIVE · ${String(activeName).toUpperCase()} → REPLACING ${String(primary?.displayName ?? primary?.name).toUpperCase()}`
            : `CONNECTED · ${String(activeName).toUpperCase()} PRIMARY`}
        {ic.status?.activeSince && <span className="since">since {new Date(ic.status.activeSince).toLocaleTimeString()}</span>}
      </div>
      <div className="cont-main">
        <div className="cont-canvas">
          <div className="cont-bar">
            {items.length > 1 && (
              <select className="field" value={pick} onChange={e => setPick(Number(e.target.value))}>
                {items.map((x, i) => <option key={i} value={i}>{x.metadata.namespace}/{x.metadata.name}</option>)}
              </select>
            )}
            <span className="grow" />
            {target && (
              <button className={cut ? 'btn ok' : 'btn danger'} disabled={busy} onClick={kill}
                title={cut ? `Remove the partition on ${target.name}` : `Cut the network path from the lab to ${target.displayName ?? target.name}`}>
                {cut ? `Restore network to ${target.displayName ?? target.name}` : `Simulate network outage · ${target.displayName ?? target.name}`}
              </button>
            )}
          </div>
          {err && <div className="note bad cont-err">{err}</div>}
          <ReactFlow nodes={rfNodes} edges={rfEdges} nodeTypes={nodeTypes} edgeTypes={edgeTypes} fitView fitViewOptions={{ padding: 0.15 }}
            nodesConnectable={false} proOptions={{ hideAttribution: true }} minZoom={0.2}>
            <Background variant={BackgroundVariant.Dots} gap={22} size={1} color="var(--border)" />
          </ReactFlow>
          <TierHealth tiers={ic.status?.tiers ?? []} />
        </div>
        <RuleBuilder ic={ic} broker={brokerNode?.label ?? 'the broker'} />
      </div>
      <div className="cont-foot">
        <div className="cont-col">
          <div className="label">Transitions</div>
          <div className="scroll trans">
            {(ic.status?.transitions ?? []).slice().reverse().map((t, i) => (
              <div key={i} className="row line">
                <span className="mono subtle">{new Date(t.time).toLocaleTimeString()}</span>
                <span className="chip">{t.from || '—'}</span>→<span className="chip accent">{t.to}</span>
                <span className="subtle ellipsis grow">{t.reason}</span>
              </div>
            ))}
            {!(ic.status?.transitions ?? []).length && <div className="subtle small">No failovers yet.</div>}
          </div>
        </div>
        <div className="cont-col grow">
          <div className="label">Identity traffic</div>
          <div className="scroll trans">
            {feed.map(t => <TrafficRow key={t.id} t={t} names={names} compact />)}
            {!feed.length && <div className="subtle small">Sign in to any app on this broker and the OIDC hops stream in here.</div>}
          </div>
        </div>
      </div>
    </div>
  )
}

function TierHealth({ tiers }: { tiers: TierStatus[] }) {
  return (
    <div className="health">
      {tiers.map(t => (
        <div key={t.name} className="hrow">
          <span className={`dot ${t.configured === false ? 'idle' : t.healthy ? 'ok' : 'bad'}`} />
          <b>{t.name}</b>
          <span className="subtle mono">{t.configured === false ? 'not configured' : t.healthy ? `${t.latencyMs ?? 0} ms` : t.reason ?? 'unhealthy'}</span>
        </div>
      ))}
    </div>
  )
}
