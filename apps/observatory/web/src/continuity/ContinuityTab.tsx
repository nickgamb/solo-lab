import { Background, BackgroundVariant, MarkerType, ReactFlow, ReactFlowProvider, type Edge, type Node } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { useMemo, useState } from 'react'
import { api, type IdentityContinuity, type Lab, type LabNode, type Tier, type TierStatus } from '../api'
import { edgeTypes, type FlowData } from '../topology/edges'
import { nodeTypes, type CardData } from '../topology/nodes'
import { CAPTION_H, LABEL_H, LABEL_W, TILE } from '../topology/layout'
import { TrafficRow } from '../traffic/TrafficRow'
import { RuleBuilder } from './RuleBuilder'
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
  const brokerNode = paths[0] ? nodes.get(paths[0].broker) : undefined
  const egress = ic.spec.egress
    ? lab.graph?.nodes.find(n => n.namespace === ic.spec.egress!.namespace && n.label === ic.spec.egress!.waypoint) : undefined
  const targetName = target?.displayName ?? target?.name ?? ''
  const host = (u?: string) => { try { return u ? new URL(u).host : '' } catch { return u ?? '' } }

  const kill = async () => {
    if (!target) return
    setBusy(true); setErr(undefined)
    try { await api('/api/continuity/partition', { method: 'POST', body: JSON.stringify({ tier: target.name, down: !cut }) }) }
    catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }

  // Left to right: every app that signs people in through this broker, the
  // broker (the issuer everything trusts), the firm's egress (where an
  // external IdP's back-channel leaves the lab, and where an outage is cut),
  // and the upstream tiers in failover order.
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
    const broker = paths[0]?.broker
    const apps = [...new Set(paths.map(p => p.app))]
    // the IdPs only: the broker's break-glass accounts aren't a hop on the map
    const idps = tiers.filter(t => t.type === 'oidc')
    const rows = Math.max(apps.length, idps.length, 1)
    const mid = (rows - 1) / 2
    apps.forEach((id, i) => {
      tile(id, nodes.get(id), 0, mid - (apps.length - 1) / 2 + i, { fog: !signInOK })
      if (broker) wire(id, broker, { state: signInOK ? 'active' : 'down' })
    })
    if (broker) tile(broker, nodes.get(broker), 1, mid, { highlight: signInOK ? 'ok' : 'bad' })
    const tierRow = (i: number) => mid - (idps.length - 1) / 2 + i
    const external = idps.map((t, i) => ({ t, i }))
    const egressRow = external.length ? external.reduce((a, x) => a + tierRow(x.i), 0) / external.length : mid
    const egressCut = external.some(x => cuts.has(x.t.name))
    const egressLive = external.some(x => x.t.name === active)
    if (egress && broker && external.length) {
      tile(egress.id, egress, 2, egressRow, { highlight: egressCut ? 'bad' : egressLive ? 'ok' : undefined })
      wire(broker, egress.id, { state: egressLive ? 'active' : 'standby', rps: egressLive ? 1 : 0 })
    }
    idps.forEach((t, i) => {
      const st = status.get(t.name)
      const isActive = t.name === active
      const isCut = cuts.has(t.name)
      const down = isCut || (!!st && st.configured !== false && !st.healthy)
      const n: LabNode = { id: `tier:${t.name}`, kind: 'idp', label: `${i + 1}. ${t.displayName ?? t.name}`, group: '',
        status: isActive ? 'ok' : down ? 'down' : 'idle', sub: host(t.oidc?.issuer), summary: {}, products: [] }
      const off = st?.configured === false || t.enabled === false || !!t.drain
      tile(n.id, n, 3, tierRow(i), { fog: !isActive && !down && off, highlight: isActive ? 'ok' : down ? 'bad' : undefined, outage: isCut, caption: n.sub })
      const from = egress ? egress.id : broker
      if (from) wire(from, n.id, { state: isActive ? 'active' : down ? 'down' : 'standby', rps: isActive ? 1 : 0,
        cut: isCut, label: isCut ? 'network cut' : undefined })
    })
    return { rfNodes, rfEdges }
  }, [tiers, status, active, cuts, nodes, paths, egress])

  const names = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n.label]) ?? []), [lab.graph])
  // this instance's identity traffic: its failovers and outages, and the
  // OIDC hops through its broker and the apps that sign in with it
  const hops = useMemo(() => new Set(paths.flatMap(p => [p.broker, p.app])), [paths])
  const feed = useMemo(() => lab.traffic.filter(t =>
    t.kind === 'continuity' ? t.reporter === key || idpNames.has(t.attrs?.idp ?? '')
      : t.kind === 'oidc' && (!hops.size || [t.source, t.target, t.via].some(id => !!id && hops.has(id)))).slice(0, 120),
  [lab.traffic, key, idpNames, hops])
  const activeName = tiers.find(t => t.name === active)?.displayName ?? active

  return (
    <div className="cont">
      <Banner phase={phase} active={activeName} primary={primary?.displayName ?? primary?.name} target={targetName || undefined} since={ic.status?.activeSince}
        reason={pst && !pst.healthy ? `${pst.reason}${pst.message ? `: ${pst.message}` : ''}` : undefined}
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
        <RuleBuilder key={key} ic={ic} broker={brokerNode?.label ?? 'the broker'} />
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
function Banner({ phase, active, primary, target, since, cut, checks, reason }: {
  phase: Phase; active?: string; primary?: string; target?: string; since?: string; cut?: { path?: string; since?: string; by?: string }; checks?: string; reason?: string
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
