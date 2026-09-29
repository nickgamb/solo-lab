import { Handle, Position, type NodeProps } from '@xyflow/react'
import { memo } from 'react'
import type { LabNode, SubActor, SubWorker } from '../api'
import { NodeIcon, ProductIcon, kindLabel, PRODUCTS } from './icons'

export type CardData = {
  n: LabNode
  rps?: number
  err?: number
  p95?: number
  selected?: boolean
  fog?: boolean
  highlight?: 'ok' | 'bad'
  workers?: SubWorker[]
  actors?: SubActor[]
  actor?: SubActor // a sandboxed agent's current session
  compact?: boolean
  lit?: boolean // highlighted by the product rail
  outage?: boolean // an IdP whose network path is cut (Identity Continuity)
  ins?: number // wires arriving (one handle each)
  outs?: number
  [key: string]: unknown
}

// Every side can start or end an edge; the canvas picks the pair that
// faces the other node, so wires don't loop around cards.
const SIDES = [['l', Position.Left], ['r', Position.Right], ['t', Position.Top], ['b', Position.Bottom]] as const

const fmt = (v?: number) => (v === undefined ? '—' : v >= 10 ? v.toFixed(0) : v.toFixed(1))

function Pods({ n }: { n: LabNode }) {
  const pods = n.pods ?? []
  if (!pods.length) return null
  return (
    <span className="pods" title={pods.map(p => `${p.name} · ${p.node ?? ''} ${p.zone ?? ''} · ${p.ready ? 'ready' : p.phase}`).join('\n')}>
      {pods.slice(0, 8).map(p => <i key={p.name} className={p.ready ? 'pd ok' : p.phase === 'Terminating' ? 'pd term' : 'pd warn'} />)}
      {pods.length > 8 && <span className="subtle">+{pods.length - 8}</span>}
    </span>
  )
}

function Products({ n }: { n: LabNode }) {
  if (!n.products?.length) return null
  return <span className="prods" title={n.products.join(', ')}>{n.products.map(p => <ProductIcon key={p} id={p} size={13} />)}</span>
}

function Shell({ d, children, className = '' }: { d: CardData; children: React.ReactNode; className?: string }) {
  const n = d.n
  const cls = ['card', `k-${n.kind}`, `s-${n.status}`, className, d.selected ? 'selected' : '', d.fog ? 'fog' : '', d.lit ? 'lit' : '', d.highlight ? `hl-${d.highlight}` : '']
  return (
    <div className={cls.join(' ')}>
      {children}
      {SIDES.map(([side, pos]) => (
        <span key={side}>
          <Handle id={`${side}-s`} type="source" position={pos} className="h" />
          <Handle id={`${side}-t`} type="target" position={pos} className="h" />
        </span>
      ))}
    </div>
  )
}

export const LabCard = memo(({ data }: NodeProps) => {
  const d = data as CardData
  const n = d.n
  if (d.compact) {
    return (
      <Shell d={d} className="compact">
        <div className="row">
          <NodeIcon n={n} size={14} />
          <span className="title ellipsis grow">{n.label}</span>
          <Pods n={n} />
        </div>
      </Shell>
    )
  }
  const gw = n.kind === 'gateway' || n.kind === 'waypoint'
  const big = n.id.endsWith('/ai-gateway')
  return (
    <Shell d={d} className={big ? 'hero' : gw ? 'gw' : ''}>
      <div className="row">
        <NodeIcon n={n} size={big ? 22 : 18} />
        <div className="grow">
          <div className="title ellipsis">{n.label}</div>
          <div className="meta ellipsis">{n.namespace ? `${n.namespace} · ` : ''}{kindLabel[n.kind] ?? n.kind}</div>
        </div>
        <Products n={n} />
        <span className={`dot ${n.status}`} />
      </div>
      {gw ? (
        <div className="gstats">
          <div><b>{fmt(d.rps)}</b><span>req/s</span></div>
          <div><b className={(d.err ?? 0) > 0 ? 'bad' : ''}>{((d.err ?? 0) * 100).toFixed(1)}%</b><span>denied/err</span></div>
          <div><b>{d.p95 ? `${Math.round(d.p95)}ms` : '—'}</b><span>p95</span></div>
        </div>
      ) : (
        <div className="row foot">
          {n.badges?.filter(b => !n.products?.includes(b)).map(b => <span key={b} className="chip accent">{b}</span>)}
          {d.actor && <span className={`chip ${d.actor.status === 'Running' ? 'ok' : d.actor.status === 'Suspended' ? '' : 'warn'}`}>{d.actor.status}</span>}
          <span className="grow" />
          <Pods n={n} />
          {(d.rps ?? 0) > 0 && <span className="rps">{fmt(d.rps)}/s</span>}
        </div>
      )}
    </Shell>
  )
})

// A WorkerPool: one bay per worker pod, showing the actor running in it.
export const SubstrateCard = memo(({ data }: NodeProps) => {
  const d = data as CardData
  const n = d.n
  const workers = d.workers ?? []
  const parked = (d.actors ?? []).filter(a => a.status === 'Suspended').length
  const bays = workers.length ? workers : (n.pods ?? []).map(p => ({ workerPod: p.name } as SubWorker))
  return (
    <Shell d={d} className="sub">
      <div className="row">
        <NodeIcon n={n} size={16} />
        <div className="grow">
          <div className="title ellipsis">{n.label}</div>
          <div className="meta ellipsis">{n.namespace} · Agent Substrate · {String(n.summary?.sandboxClass ?? 'gvisor')}</div>
        </div>
        <span className={`dot ${n.status}`} />
      </div>
      <div className="bays">
        {bays.map(w => {
          const a = (d.actors ?? []).find(x => x.actorId && x.actorId === w.actorId)
          return (
            <div key={w.workerPod} className={a ? `bay busy ${a.status.toLowerCase()}` : 'bay'} title={w.workerPod}>
              {a ? <span className="ellipsis">{a.actorTemplateName.replace(/-[a-z0-9]{6,}$/, '')}</span> : <span className="subtle">idle</span>}
            </div>
          )
        })}
      </div>
      <div className="row foot">
        <span className="chip">{parked} snapshot{parked === 1 ? '' : 's'} parked</span>
        <span className="grow" />
        <Pods n={n} />
      </div>
    </Shell>
  )
})

export type GroupData = {
  label: string; domain?: string; fog?: boolean; outside?: boolean
  hits?: number; product?: string // instances of the rail's product folded into this zone
  stats?: { agents: number; workloads: number; ready: number; pods: number; rps: number; err: number; idle: number; down: number }
  onOpen?: () => void
  [key: string]: unknown
}

// PartyBox: a zone, with a live status bar across its top. Clicking the bar
// opens the zone's details (what's running there with nothing on the map).
export const PartyBox = memo(({ data }: NodeProps) => {
  const d = data as GroupData
  const st = d.stats
  return (
    <div className={['party', d.fog ? 'fog' : '', d.outside ? 'outside' : ''].join(' ')}>
      <button className="party-bar nodrag nopan" onClick={e => { e.stopPropagation(); d.onOpen?.() }} title={d.outside ? undefined : 'Zone details'} disabled={d.outside}>
        <span className="party-name">{d.label}</span>
        {d.domain && <span className="subtle mono">{d.domain}</span>}
        <span className="grow" />
        {st && (
          <span className="party-stats">
            {!!d.hits && d.product && (
              <span className="zhit" title={`${d.hits} ${PRODUCTS.find(p => p.id === d.product)?.label ?? d.product} workloads in this zone, not drawn as tiles: open the zone to list them`}>
                <ProductIcon id={d.product} size={15} /><b>{d.hits}</b> in this zone
              </span>
            )}
            {st.agents > 0 && <span><b>{st.agents}</b> agents</span>}
            <span><b>{st.workloads}</b> workloads</span>
            <span className={st.ready < st.pods ? 'warn' : ''}><b>{st.ready}/{st.pods}</b> pods ready</span>
            <span><b>{st.rps >= 10 ? st.rps.toFixed(0) : st.rps.toFixed(1)}</b> req/s</span>
            <span className={st.err > 0 ? 'bad' : ''}><b>{(st.err * 100).toFixed(1)}%</b> err</span>
            <span className="subtle">{st.idle > 0 ? <><b>{st.idle}</b> idle </> : null}details ›</span>
          </span>
        )}
      </button>
    </div>
  )
})

// Tile: a node as an icon tile with its name underneath (n8n-style), so the
// whole lab reads at a glance. Wires leave the tile's right side and arrive
// at its left.
export const Tile = memo(({ data }: NodeProps) => {
  const d = data as CardData
  const n = d.n
  const hero = n.kind === 'gateway' && !!n.products?.includes('agentgateway')
  const lead = PRODUCTS.find(p => n.products?.includes(p.id))?.id
  const members = n.summary?.members as string[] | undefined
  const cls = ['tile', `k-${n.kind}`, `s-${n.status}`, hero ? 'hero' : '', members ? 'stack' : '', d.selected ? 'selected' : '', d.fog ? 'fog' : '', d.lit ? 'lit' : '', d.highlight ? `hl-${d.highlight}` : '']
  const rps = d.rps ?? 0
  const busy = (d.workers ?? []).filter(w => w.actorId).length
  return (
    <div className={cls.join(' ')}>
      <div className="tbox">
        <Slots d={d} />
        {lead ? <ProductIcon id={lead} size={hero ? 50 : 38} /> : <NodeIcon n={n} size={hero ? 50 : 38} />}
        {members && <span className="count">{members.length}</span>}
        {!members && (n.pods?.length ?? 0) > 1 && <span className="replicas" title={`${n.pods!.length} replicas`}>×{n.pods!.length}</span>}
        {d.actor && <span className={`corner ${d.actor.status === 'Running' ? 'ok' : ''}`} title={`sandbox ${d.actor.status}`}><ProductIcon id="substrate" size={11} /></span>}
        {n.kind === 'substrate' && d.workers && <span className="bays-mini">{d.workers.map(w => <i key={w.workerPod} className={w.actorId ? 'on' : ''} />)}</span>}
        {rps > 0 && <span className="trps">{fmt(rps)}/s</span>}
        {d.outage && <span className="stamp">OUTAGE</span>}
      </div>
      <div className="tlabel" title={n.label}>{n.label}</div>
      <div className="tsub">
        {hero ? `${fmt(rps)} req/s · ${((d.err ?? 0) * 100).toFixed(1)}% · ${d.p95 ? `${Math.round(d.p95)}ms` : '— ms'}`
          : n.kind === 'substrate' && d.workers ? `${busy}/${d.workers.length} busy` : kindLabel[n.kind] ?? n.kind}
      </div>
    </div>
  )
})

// Slots: wires leave from the right and arrive on the left; a call to
// something further left leaves from the left and arrives on the right.
function Slots(_: { d: CardData }) {
  return (
    <>
      <Handle id="i0" type="target" position={Position.Left} className="h" />
      <Handle id="o0" type="source" position={Position.Right} className="h" />
      <Handle id="ol" type="source" position={Position.Left} className="h" />
      <Handle id="ir" type="target" position={Position.Right} className="h" />
    </>
  )
}

// Chip: platform plumbing, shown only when asked for.
export const Chip = memo(({ data }: NodeProps) => {
  const d = data as CardData
  const n = d.n
  return (
    <div className={['chipnode', d.selected ? 'selected' : '', d.fog ? 'fog' : '', d.lit ? 'lit' : ''].join(' ')}>
      <Slots d={d} />
      <span className={`dot ${n.status}`} />
      <span className="ellipsis grow" title={`${n.label}: no traffic on the layers shown`}>{n.label}</span>
    </div>
  )
})

// Port: where bundled wires enter a party.
export const Port = memo(({ data }: NodeProps) => (
  <div className="port">
    <Slots d={data as CardData} />
  </div>
))

// Tray: a Substrate worker pool, drawn around the agents it runs, with a
// bay per worker (lit while it holds an agent's session).
export const TrayBox = memo(({ data }: NodeProps) => {
  const d = data as CardData
  const ws = d.workers ?? []
  const busy = ws.filter(w => w.actorId).length
  return (
    <div className={['tray', d.selected ? 'selected' : '', d.fog ? 'fog' : '', d.lit ? 'lit' : ''].join(' ')}>
      <div className="tray-head">
        <ProductIcon id="substrate" size={15} />
        <span className="tray-name ellipsis" title={`Agent Substrate worker pool ${d.n.label}`}>{d.n.label.replace(/-deployment$/, '')}</span>
        <span className="grow" />
        {ws.length > 0 && <span className="bays-mini">{ws.map(w => <i key={w.workerPod} className={w.actorId ? 'on' : ''} title={w.workerPod} />)}</span>}
        <span className="subtle mono">{ws.length ? `${busy}/${ws.length} busy` : ''}</span>
      </div>
    </div>
  )
})

// Door: the internet-facing gateway as a pillar down the left of the map;
// every zone's way in leaves it level with what it reaches.
export const DoorPillar = memo(({ data }: NodeProps) => {
  const d = data as CardData
  const n = d.n
  const lead = PRODUCTS.find(p => n.products?.includes(p.id))?.id
  return (
    <div className={['door', `s-${n.status}`, d.selected ? 'selected' : '', d.fog ? 'fog' : '', d.lit ? 'lit' : ''].join(' ')}>
      <Slots d={d} />
      <div className="door-cap">
        {lead ? <ProductIcon id={lead} size={40} /> : <NodeIcon n={n} size={40} />}
        {(d.rps ?? 0) > 0 && <span className="trps">{fmt(d.rps)}/s</span>}
      </div>
      <div className="door-label"><b>{n.label}</b><span>{kindLabel[n.kind]} · the way in</span></div>
    </div>
  )
})

// ColHead: the stage a column holds, above the lanes.
export const ColHead = memo(({ data }: NodeProps) => {
  const d = data as { label: string; step: number }
  return <div className="colhead">{d.label}</div>
})

export const nodeTypes = { card: LabCard, substrate: SubstrateCard, party: PartyBox, tile: Tile, chip: Chip, port: Port, tray: TrayBox, colhead: ColHead, door: DoorPillar }
