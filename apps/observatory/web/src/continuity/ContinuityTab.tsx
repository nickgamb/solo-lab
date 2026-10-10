import { Background, BackgroundVariant, MarkerType, ReactFlow, ReactFlowProvider, type Edge, type Node } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { useMemo, useState } from 'react'
import { api, type IdentityContinuity, type Lab, type LabNode, type Tier, type TierStatus } from '../api'
import { edgeTypes, type FlowData } from '../topology/edges'
import { nodeTypes, type CardData } from '../topology/nodes'
import { CAPTION_H, LABEL_H, LABEL_W, TILE } from '../topology/layout'
import { TrafficRow } from '../traffic/TrafficRow'
import { RuleBuilder } from './RuleBuilder'
import { enforced } from './rulesCode'
import './continuity.css'

const instanceKey = (ic: IdentityContinuity) => `${ic.metadata.namespace}/${ic.metadata.name}`
const NO_TIERS: Tier[] = []

export function ContinuityTab({ lab }: { lab: Lab }) {
  const items = lab.continuity?.items ?? []
  // by namespace/name, so the choice holds as instances come and go
  const [pick, setPick] = useState<string>()
  const ic = items.find(x => instanceKey(x) === pick) ?? items[0]
  if (!ic) {
    return <div className="cont-empty subtle">No IdentityContinuity resources found. Create one and it appears here.</div>
  }
  return (
    <ReactFlowProvider>
      {/* keyed by instance: switching instances starts fresh (unsaved rule
          edits never carry over to another IdentityContinuity) */}
      <Continuity key={instanceKey(ic)} lab={lab} ic={ic} items={items} setPick={setPick} />
    </ReactFlowProvider>
  )
}

function Continuity({ lab, ic, items, setPick }: {
  lab: Lab; ic: IdentityContinuity; items: IdentityContinuity[]; setPick: (key: string) => void
}) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string>()
  const [choice, setChoice] = useState<string>() // which upstream the outage button cuts
  const tiers = ic.spec.tiers ?? NO_TIERS
  const status = useMemo(() => new Map((ic.status?.tiers ?? []).map(t => [t.name, t])), [ic.status?.tiers])
  const active = ic.status?.active
  const primary = tiers.find(t => t.enabled !== false && !t.drain && status.get(t.name)?.configured !== false)
  // this instance's IdPs: cuts and identity traffic for another instance's aren't shown
  const idpNames = useMemo(() => new Set(tiers.filter(t => t.type === 'oidc').map(t => t.name)), [tiers])
  const partitions = lab.continuity?.partitions
  const cuts = useMemo(() => new Map((partitions ?? []).filter(p => idpNames.has(p.tier)).map(p => [p.tier, p])), [partitions, idpNames])
  const upstreams = tiers.filter(t => t.type === 'oidc')
  const target = upstreams.find(t => cuts.has(t.name)) ?? upstreams.find(t => t.name === choice) ?? tiers.find(t => t.type === 'oidc' && t.name === active)
    ?? tiers.find(t => t.type === 'oidc' && status.get(t.name)?.configured !== false) ?? tiers.find(t => t.type === 'oidc')
  const cut = target ? cuts.get(target.name) : undefined
  const failover = !!active && !!primary && active !== primary.name
  const health = ic.spec.health ?? {}
  const pst = primary ? status.get(primary.name) : undefined
  // where the story is: a cut the controller hasn't reacted to yet, a
  // failover, the primary answering again while the controller verifies it,
  // or a healthy primary waiting on a manual failback
  const phase: Phase =
    !active ? 'down'
      : cut && active === target?.name ? 'detecting'
        : failover && !cut && pst && !pst.healthy && (pst.consecutiveSuccesses ?? 0) > 0 ? 'recovering'
          : failover && pst?.healthy && ic.spec.failback === 'Manual' ? 'held'
            : failover ? 'failover' : 'ok'
  const nodes = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n]) ?? []), [lab.graph])
  const key = instanceKey(ic)
  const paths = useMemo(() => (lab.continuity?.paths ?? []).filter(p => p.instance === key), [lab.continuity?.paths, key])
  const resources = useMemo(() => (lab.continuity?.resources ?? []).filter(p => p.instance === key), [lab.continuity?.resources, key])
  const groups = useMemo(() => new Map(lab.graph?.groups.map(g => [g.id, g]) ?? []), [lab.graph])
  const brokerID = paths[0]?.broker ?? resources[0]?.broker
  const targetName = target?.displayName ?? target?.name ?? ''
  const host = (u?: string) => { try { return u ? new URL(u).host : '' } catch { return u ?? '' } }

  const kill = async () => {
    if (!target) return
    setBusy(true); setErr(undefined)
    try { await api('/api/continuity/partition', { method: 'POST', body: JSON.stringify({ tier: target.name, down: !cut }) }) }
    catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }

  // Left to right: the resources S&V's gateways admit the fabric's tokens to
  // (another party's, such as a SaaS, as one tile) and the apps people sign
  // in to; the gateways every path comes in through; the broker attached to
  // them; and the IdPs in failover order. An outage is cut at the firm's
  // egress, on the wire to that IdP.
  const { rfNodes, rfEdges } = useMemo(() => {
    const rfNodes: Node[] = []
    const rfEdges: Edge[] = []
    const COL = 330, ROW = 170
    const signInOK = !!active
    // Nodes are rebuilt on every update. React Flow keeps a node's measured
    // handle positions only when the node says it is measured; without that,
    // each rebuild drops them and its wires vanish until something re-measures
    // (which may never happen on an idle page). Every size here is fixed.
    const tile = (id: string, n: LabNode | undefined, col: number, row: number, extra: Partial<CardData> = {}) => {
      if (!n) return
      const height = TILE + LABEL_H + 4 + (extra.caption ? CAPTION_H : 0)
      rfNodes.push({ id, type: 'tile', position: { x: col * COL, y: row * ROW }, data: { n, ...extra } as CardData,
        width: LABEL_W, height, style: { width: LABEL_W, height }, draggable: false,
        measured: { width: LABEL_W, height } })
    }
    const wire = (s: string, t: string, data: Partial<FlowData>) => rfEdges.push({ id: `${s}>${t}`, source: s, target: t, type: 'flow',
      sourceHandle: 'o0', targetHandle: 'i0', data: { kind: 'oidc', ...data } as FlowData,
      markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14, color: data.state === 'down' ? 'var(--bad)' : data.state === 'active' ? 'var(--ok)' : 'var(--text-subtle)' } })
    const broker = brokerID
    const apps = [...new Set(paths.map(p => p.app))]
    // the firm's own resources one by one; another party's as one tile
    const home = broker ? nodes.get(broker)?.group : undefined
    const res = new Map<string, { n: LabNode; gateways: string[] }>()
    for (const p of resources) {
      const n = nodes.get(p.resource)
      if (!n || !nodes.get(p.gateway)) continue
      const own = !n.group || n.group === home
      const id = own ? n.id : `party:${n.group}`
      let r = res.get(id)
      if (!r) {
        const g = groups.get(n.group)
        r = { n: own ? n : { id, kind: 'external', label: g?.label ?? n.group, group: n.group, status: 'ok', sub: g?.domain, summary: {}, products: [] }, gateways: [] }
        res.set(id, r)
      }
      if (!r.gateways.includes(p.gateway)) r.gateways.push(p.gateway)
    }
    const gateways = [...new Set([...res.values()].flatMap(r => r.gateways))]
    // the IdPs only: the broker's break-glass accounts aren't a hop on the map
    const idps = tiers.filter(t => t.type === 'oidc')
    const left = res.size + apps.length
    const rows = Math.max(left, idps.length, gateways.length, 1)
    const mid = (rows - 1) / 2
    const entryCol = paths.some(p => p.entry && nodes.get(p.entry))
    const col = gateways.length || entryCol ? { broker: 2, idp: 3 } : { broker: 1, idp: 2 }
    const rowOf = new Map<string, number>()
    let row = mid - (left - 1) / 2
    for (const [id, r] of res) {
      rowOf.set(id, row)
      tile(id, r.n, 0, row++, { fog: !signInOK, caption: id.startsWith('party:') ? r.n.sub : undefined })
      for (const gw of r.gateways) wire(id, gw, { kind: 'mcp', state: signInOK ? 'active' : 'down' })
    }
    // apps people sign in to come in through the edge, where it signs them in
    const entry = paths.find(p => p.entry && nodes.get(p.entry))?.entry
    for (const id of apps) {
      rowOf.set(id, row)
      tile(id, nodes.get(id), 0, row++, { fog: !signInOK })
      const via = entry ?? broker
      if (via) wire(id, via, { state: signInOK ? 'active' : 'down' })
    }
    // each gateway level with what comes in through it
    const taken = new Set<number>()
    const place = (gw: string, from: string[]) => {
      const rs = from.map(id => rowOf.get(id) ?? mid)
      let r = rs.reduce((a, x) => a + x, 0) / Math.max(rs.length, 1)
      while (taken.has(r)) r += 1
      taken.add(r)
      tile(gw, nodes.get(gw), 1, r, { highlight: signInOK ? 'ok' : 'bad' })
      if (broker) wire(gw, broker, { state: signInOK ? 'active' : 'down' })
    }
    for (const gw of gateways) place(gw, [...res].filter(([, r]) => r.gateways.includes(gw)).map(([id]) => id))
    if (entry && apps.length && !gateways.includes(entry)) place(entry, apps)
    // the broker, drawn as the identity fabric (the graph marks it)
    const fabric = broker ? nodes.get(broker) : undefined
    if (broker && fabric) tile(broker, fabric, col.broker, mid, { highlight: signInOK ? 'ok' : 'bad', caption: fabric.sub })
    const tierRow = (i: number) => mid - (idps.length - 1) / 2 + i
    // the IdPs routing rules send sign-ins to now sign people in too
    const routedBy = new Map<string, string[]>()
    for (const r of ic.status?.routing ?? []) if (r.idp && r.idp !== active) routedBy.set(r.idp, [...(routedBy.get(r.idp) ?? []), r.name])
    idps.forEach((t, i) => {
      const st = status.get(t.name)
      const routedHere = routedBy.get(t.name)
      const isActive = t.name === active || !!routedHere
      const isCut = cuts.has(t.name)
      const down = isCut || (!!st && st.configured !== false && !st.healthy)
      const n: LabNode = { id: `tier:${t.name}`, kind: 'idp', label: `${i + 1}. ${t.displayName ?? t.name}`, group: '',
        status: isActive ? 'ok' : down ? 'down' : 'idle', sub: host(t.oidc?.issuer), summary: {}, products: [] }
      const off = st?.configured === false || t.enabled === false || !!t.drain
      tile(n.id, n, col.idp, tierRow(i), { fog: !isActive && !down && off, highlight: isActive ? 'ok' : down ? 'bad' : undefined, outage: isCut, caption: n.sub })
      if (broker) wire(broker, n.id, { state: isActive ? 'active' : down ? 'down' : 'standby', rps: isActive ? 1 : 0,
        cut: isCut, label: isCut ? 'network cut' : routedHere ? `routed: ${routedHere.join(', ')}` : undefined })
    })
    return { rfNodes, rfEdges }
  }, [tiers, status, active, cuts, nodes, paths, resources, groups, brokerID, ic.status?.routing])

  const names = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n.label]) ?? []), [lab.graph])
  // this instance's identity traffic: its failovers and outages, and the
  // OIDC hops through its broker and the apps that sign in with it
  const hops = useMemo(() => new Set(paths.flatMap(p => [p.broker, p.app])), [paths])
  const feed = useMemo(() => lab.traffic.filter(t =>
    t.kind === 'continuity' ? t.reporter === key || idpNames.has(t.attrs?.idp ?? '')
      : t.kind === 'oidc' && (!hops.size || [t.source, t.target, t.via].some(id => !!id && hops.has(id)))).slice(0, 120),
  [lab.traffic, key, idpNames, hops])
  const activeName = tiers.find(t => t.name === active)?.displayName ?? active
  // this instance's resources with assurance rules, and those the active IdP can't meet
  const profiles = useMemo(() => (lab.continuity?.profiles ?? []).filter(p => p.namespace === ic.metadata.namespace && p.continuity === ic.metadata.name),
    [lab.continuity?.profiles, ic.metadata.namespace, ic.metadata.name])
  const closed = profiles.filter(p => p.phase === 'FailedClosed' && enforced(p)).map(p => p.name)

  return (
    <div className="cont">
      <Banner phase={phase} active={activeName} primary={primary?.displayName ?? primary?.name} target={targetName || undefined} since={ic.status?.activeSince}
        reason={pst && !pst.healthy ? `${pst.reason}${pst.message ? `: ${pst.message}` : ''}` : undefined} closed={closed}
        cut={cut} checks={phase === 'recovering' ? `${pst?.consecutiveSuccesses ?? 0}/${health.healthyThreshold ?? 3} healthy checks`
          : phase === 'detecting' ? `${status.get(target?.name ?? '')?.consecutiveFailures ?? 0}/${health.unhealthyThreshold ?? 2} failed checks` : undefined} />
      <div className="cont-main">
        <div className="cont-canvas">
          <div className="cont-bar">
            {items.length > 1 && (
              <select className="field" aria-label="IdentityContinuity" value={key} onChange={e => setPick(e.target.value)}>
                {items.map(x => <option key={instanceKey(x)} value={instanceKey(x)}>{instanceKey(x)}</option>)}
              </select>
            )}
            <span className="grow" />
            {target && (
              <div className="outage">
                <div className="row">
                  {upstreams.length > 1 && !cut && (
                    <select className="field" value={target.name} onChange={e => setChoice(e.target.value)} title="the upstream IdP to cut off" aria-label="IdP to cut off">
                      {upstreams.map(t => <option key={t.name} value={t.name}>{t.displayName ?? t.name}</option>)}
                    </select>
                  )}
                  <button className={cut ? 'btn big ok' : 'btn big danger'} disabled={busy} onClick={kill}
                    title={cut ? `Remove the DENY on the path to ${targetName}` : `A real Istio DENY on the path from the lab to ${targetName}`}>
                    {cut ? 'Restore IdP network' : 'Simulate IdP outage'}
                  </button>
                </div>
                <span className="subtle small">{cut ? `${targetName} cut ${cut.since ? new Date(cut.since).toLocaleTimeString() : ''}${cut.by ? ` by ${cut.by}` : ''} · ${cut.path ?? ''}`
                  : `cuts ${targetName} (${host(target.oidc?.issuer)}) at the ${ic.spec.egress?.namespace ?? 'firm'} egress`}</span>
              </div>
            )}
          </div>
          {err && <div className="note bad cont-err">{err}</div>}
          <ReactFlow nodes={rfNodes} edges={rfEdges} nodeTypes={nodeTypes} edgeTypes={edgeTypes} fitView fitViewOptions={{ padding: 0.15 }}
            nodesConnectable={false} proOptions={{ hideAttribution: true }} minZoom={0.2}>
            <Background variant={BackgroundVariant.Dots} gap={22} size={1} color="var(--border)" />
          </ReactFlow>
          <TierHealth tiers={tiers} status={status} />
        </div>
        <RuleBuilder key={key} ic={ic} broker="the identity fabric" profiles={profiles} />
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

type Phase = 'ok' | 'detecting' | 'failover' | 'recovering' | 'held' | 'down'

// Banner: the one line a room reads from the back: which IdP is signing
// people in, and, during an outage, what happened and what the lab did.
// target is the IdP the outage button cuts.
function Banner({ phase, active, primary, target, since, cut, checks, reason, closed }: {
  phase: Phase; active?: string; primary?: string; target?: string; since?: string; cut?: { path?: string; since?: string; by?: string }; checks?: string; reason?: string; closed: string[]
}) {
  const up = (x?: string) => String(x ?? '').toUpperCase()
  const cls = phase === 'ok' ? 'ok' : phase === 'detecting' || phase === 'recovering' || phase === 'held' ? 'warn' : 'bad'
  const head = {
    ok: `CONNECTED · ${up(active)} SIGNING PEOPLE IN`,
    detecting: `OUTAGE · ${up(target ?? primary)} UNREACHABLE · FAILING OVER`,
    failover: `FAILOVER ACTIVE · ${up(active)} → REPLACING ${up(primary)}`,
    recovering: `${up(primary)} ANSWERING AGAIN · VERIFYING BEFORE FAILING BACK`,
    held: `${up(active)} SIGNING PEOPLE IN · ${up(primary)} HEALTHY, FAILBACK IS MANUAL`,
    down: 'SIGN-IN UNAVAILABLE · NO HEALTHY IDP',
  }[phase]
  const why = phase === 'ok' || phase === 'held' ? undefined
    : cut ? `network to ${cut.path ?? target ?? primary} cut${cut.by ? ` by ${cut.by}` : ''}${cut.since ? ` at ${new Date(cut.since).toLocaleTimeString()}` : ''}`
      : phase === 'recovering' ? 'the controller waits for steady health checks'
        : reason
  return (
    <div className={`banner ${cls} p-${phase}`}>
      <span className={`dot ${cls}`} />
      <span className="bhead">{head}</span>
      {checks && <span className="chip">{checks}</span>}
      {!!closed.length && <span className="chip bad" title={`their assurance rules can't be met by sign-ins through ${active ?? 'no IdP'}: requests to them are refused`}>failing closed: {closed.join(', ')}</span>}
      {why && <span className="why">{why}</span>}
      {since && phase !== 'detecting' && <span className="since">active since {new Date(since).toLocaleTimeString()}</span>}
    </div>
  )
}

// TierHealth: each IdP's last probe, in failover order. The broker's
// break-glass accounts are not an IdP, so they aren't listed.
function TierHealth({ tiers, status }: { tiers: Tier[]; status: Map<string, TierStatus> }) {
  return (
    <div className="health">
      {tiers.filter(t => t.type !== 'local').map(t => {
        const st = status.get(t.name)
        return (
          <div key={t.name} className="hrow">
            <span className={`dot ${!st || st.configured === false ? 'idle' : st.healthy ? 'ok' : 'bad'}`} />
            <b title={t.name}>{t.displayName ?? t.name}</b>
            <span className="subtle mono">{!st ? 'not probed yet' : st.configured === false ? 'not configured' : st.healthy ? `${st.latencyMs ?? 0} ms` : st.reason ?? 'unhealthy'}</span>
          </div>
        )
      })}
    </div>
  )
}
