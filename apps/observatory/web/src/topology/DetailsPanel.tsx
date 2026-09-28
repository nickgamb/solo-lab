import { Component, lazy, Suspense, useMemo, useState, type ReactNode } from 'react'
import { refKey, type Lab, type LabNode, type Ref } from '../api'
import { TrafficRow } from '../traffic/TrafficRow'
import { NodeIcon, kindLabel } from './icons'
import { actorFor, substrateFor } from './Topology'

// Monaco is large; load it the first time someone opens Advanced.
const ConfigEditor = lazy(() => import('./ConfigEditor').then(m => ({ default: m.ConfigEditor })))

// DetailsPanel: what a node is, what it runs, and only the traffic that
// touched it. "Advanced" swaps the summary for the object's live YAML.
export function DetailsPanel({ lab, node, onClose, onOpenNode, onExpand }: { lab: Lab; node: LabNode; onClose: () => void; onOpenNode: (id: string) => void; onExpand?: (id: string) => void }) {
  const [advanced, setAdvanced] = useState(false)
  const [editRef, setEditRef] = useState<Ref | undefined>(node.ref)
  const [dir, setDir] = useState<'all' | 'in' | 'out'>('all')
  const names = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n.label]) ?? []), [lab.graph])

  const self = useMemo(() => new Set([node.id, ...((node.summary?.members as string[] | undefined) ?? [])]), [node])
  const traffic = useMemo(() => lab.traffic.filter(t => {
    const out = !!t.source && self.has(t.source)
    const inn = (!!t.target && self.has(t.target)) || (!!t.via && self.has(t.via))
    return dir === 'all' ? out || inn : dir === 'in' ? inn : out
  }).slice(0, 200), [lab.traffic, self, dir])

  const refs = [node.ref, ...(node.related ?? [])].filter(Boolean) as Ref[]
  const edges = lab.graph?.edges.filter(e => e.source === node.id || e.target === node.id) ?? []

  return (
    <aside className={advanced ? 'details wide' : 'details'}>
      <header className="dhead">
        <NodeIcon n={node} size={22} />
        <div className="grow">
          <div className="row"><h3 className="ellipsis">{node.label}</h3><span className="chip accent">{node.summary?.zone === true ? 'Zone' : kindLabel[node.kind] ?? node.kind}</span></div>
          <div className="subtle mono ellipsis">{node.namespace ?? node.group.replace('party:', '')}{node.sub ? ` · ${node.sub}` : ''}</div>
        </div>
        {refs.length > 0 && (
          <button className={advanced ? 'btn small primary' : 'btn small'} onClick={() => { setAdvanced(v => !v); setEditRef(node.ref ?? refs[0]) }}>
            {advanced ? 'Summary' : 'Advanced'}
          </button>
        )}
        <button className="btn ghost small" onClick={onClose} aria-label="Close">✕</button>
      </header>

      {advanced && editRef ? (
        <div className="dbody editor-body">
          <select className="field" value={refKey(editRef)} onChange={e => setEditRef(refs.find(r => refKey(r) === e.target.value))}>
            {refs.map(r => <option key={refKey(r)} value={refKey(r)}>{r.kind} · {r.namespace ? `${r.namespace}/` : ''}{r.name}</option>)}
          </select>
          <Guard key={refKey(editRef)}>
            <Suspense fallback={<div className="subtle small">Loading editor…</div>}>
              <ConfigEditor key={refKey(editRef)} target={editRef} />
            </Suspense>
          </Guard>
        </div>
      ) : (
        <div className="dbody scroll">
          {node.summary?.zone === true && <ZoneSummary lab={lab} zone={node} onOpenNode={onOpenNode} />}
          {Array.isArray(node.summary?.members) && node.summary?.zone !== true && (
            <section>
              <div className="row"><div className="label grow">In this stack</div>{onExpand && <button className="btn small" onClick={() => onExpand(node.id)}>Unfold on the map</button>}</div>
              {(node.summary!.members as string[]).map(id => (
                <button key={id} className="conn" onClick={() => onOpenNode(id)}>
                  <span className="grow ellipsis">{names.get(id) ?? id}</span><span className="subtle">open</span>
                </button>
              ))}
            </section>
          )}
          {node.summary?.zone !== true && <section>
            <div className="label">Summary</div>
            <dl className="kv">
              <dt>Status</dt><dd><span className={`dot ${node.status}`} /> {node.status}</dd>
              {Object.entries(node.summary ?? {}).filter(([k]) => !['tools', 'nodes', 'members', 'zone', 'idle', 'stats'].includes(k)).map(([k, v]) => (
                <Fragment key={k} k={k} v={v} />
              ))}
            </dl>
          </section>}

          {node.kind === 'substrate' && lab.substrate && <SubstratePool lab={lab} node={node} />}
          {node.kind === 'agent' && node.badges?.includes('substrate') && lab.substrate && <SandboxState lab={lab} node={node} />}

          {Array.isArray(node.summary?.tools) && (
            <section>
              <div className="label">Tools</div>
              {(node.summary!.tools as { server: string; tools?: string[]; requireApproval?: string[]; allowedHeaders?: string[] }[]).map(t => (
                <div key={t.server} className="toolset">
                  <div className="row"><b>{t.server}</b>{t.allowedHeaders?.length ? <span className="chip">forwards {t.allowedHeaders.join(', ')}</span> : null}</div>
                  <div className="tools">
                    {(t.tools ?? []).map(x => <span key={x} className={t.requireApproval?.includes(x) ? 'chip warn' : 'chip'} title={t.requireApproval?.includes(x) ? 'needs human approval' : ''}>{x}</span>)}
                  </div>
                </div>
              ))}
            </section>
          )}

          {Array.isArray(node.summary?.nodes) && (
            <section>
              <div className="label">Nodes</div>
              {(node.summary!.nodes as { name: string; zone: string; ready: boolean; cpu: string; memory: string }[]).map(m => (
                <div key={m.name} className="row line"><span className={m.ready ? 'dot ok' : 'dot bad'} /><span className="grow mono">{m.name}</span><span className="subtle">{m.zone}</span><span className="subtle">{m.cpu} cpu</span></div>
              ))}
            </section>
          )}

          {!!node.pods?.length && node.summary?.zone !== true && (
            <section>
              <div className="label">Pods · {node.pods.length}</div>
              {node.pods.map(p => (
                <div key={p.name} className="row line" title={p.ip}>
                  <span className={p.ready ? 'dot ok' : 'dot warn'} />
                  <span className="grow mono ellipsis">{p.name}</span>
                  <span className="subtle">{p.zone ?? p.node}</span>
                  {p.restarts > 0 && <span className="chip warn">{p.restarts} restarts</span>}
                </div>
              ))}
            </section>
          )}

          {!!node.identity?.length && (
            <section>
              <div className="label">Mesh identity</div>
              {node.identity.map(i => <div key={i} className="mono small ellipsis" title={i}>{i}</div>)}
            </section>
          )}

          {edges.length > 0 && (
            <section>
              <div className="label">Connections</div>
              {edges.map(e => {
                const other = e.source === node.id ? e.target : e.source
                return (
                  <button key={e.id} className="conn" onClick={() => onOpenNode(other)}>
                    <span className="subtle">{e.source === node.id ? '→' : '←'}</span>
                    <span className="grow ellipsis">{names.get(other) ?? other}</span>
                    <span className="chip" style={{ color: `var(--k-${e.kind})` }}>{e.kind}</span>
                    {e.observed && <span className="chip ok" title="seen on the wire by the mesh">seen</span>}
                  </button>
                )
              })}
            </section>
          )}

          {refs.length > 0 && (
            <section>
              <div className="label">Configuration</div>
              {refs.map(r => (
                <button key={refKey(r)} className="conn" onClick={() => { setEditRef(r); setAdvanced(true) }}>
                  <span className="chip">{r.kind}</span><span className="grow mono ellipsis">{r.namespace ? `${r.namespace}/` : ''}{r.name}</span><span className="subtle">edit</span>
                </button>
              ))}
            </section>
          )}

          <section>
            <div className="row">
              <div className="label grow">Traffic</div>
              <div className="seg small">
                {(['all', 'in', 'out'] as const).map(d => <button key={d} className={dir === d ? 'on' : ''} onClick={() => setDir(d)}>{d}</button>)}
              </div>
            </div>
            {traffic.length === 0 && <div className="subtle small">Nothing yet. Requests through or to this node appear here live.</div>}
            <div className="tlist">
              {traffic.map(t => <TrafficRow key={t.id} t={t} names={names} self={node.id} compact />)}
            </div>
          </section>
        </div>
      )}
    </aside>
  )
}

// Guard keeps an editor failure inside the panel instead of the whole page.
class Guard extends Component<{ children: ReactNode }, { err?: Error }> {
  state: { err?: Error } = {}
  static getDerivedStateFromError(err: Error) { return { err } }
  render() {
    return this.state.err ? <div className="note bad">The editor failed to load: {this.state.err.message}</div> : this.props.children
  }
}

function Fragment({ k, v }: { k: string; v: unknown }) {
  if (v === '' || v === undefined || v === null || (Array.isArray(v) && !v.length)) return null
  const text = Array.isArray(v) ? v.join(', ') : typeof v === 'object' ? JSON.stringify(v) : String(v)
  return (<><dt>{k.replace(/([A-Z])/g, ' $1').toLowerCase()}</dt><dd className={text.length > 40 ? 'mono small' : ''}>{text}</dd></>)
}

function SubstratePool({ lab, node }: { lab: Lab; node: LabNode }) {
  const { workers, actors } = substrateFor(lab.substrate!, node)
  const byTemplate = new Map<string, number>()
  for (const a of actors.filter(a => a.status === 'Suspended')) byTemplate.set(a.actorTemplateName, (byTemplate.get(a.actorTemplateName) ?? 0) + 1)
  return (
    <section>
      <div className="label">Workers</div>
      {workers.map(w => {
        const a = actors.find(x => x.actorId === w.actorId)
        return (
          <div key={w.workerPod} className="row line">
            <span className={a ? 'dot ok' : 'dot idle'} />
            <span className="grow mono ellipsis">{w.workerPod}</span>
            <span className={a ? 'chip ok' : 'chip'}>{a ? `${a.actorTemplateName} · ${a.status}` : 'idle'}</span>
          </div>
        )
      })}
      <div className="label" style={{ marginTop: 12 }}>Snapshots in object storage</div>
      {byTemplate.size === 0 && <div className="subtle small">No suspended actors.</div>}
      {[...byTemplate].map(([t, c]) => <div key={t} className="row line"><span className="grow mono ellipsis">{t}</span><span className="chip">{c} suspended</span></div>)}
    </section>
  )
}

function SandboxState({ lab, node }: { lab: Lab; node: LabNode }) {
  const a = actorFor(lab.substrate!, node)
  const sessions = lab.substrate!.actors.filter(x => x.actorTemplateNamespace === node.namespace && x.actorTemplateName.startsWith(node.label))
  return (
    <section>
      <div className="label">Sandbox</div>
      <dl className="kv">
        <dt>sessions</dt><dd>{sessions.length} ({sessions.filter(s => s.status === 'Running').length} running)</dd>
        {a && <><dt>latest</dt><dd>{a.status}{a.ateomPodName ? ` on ${a.ateomPodName}` : ''}</dd></>}
        {a?.latestSnapshot && <><dt>snapshot</dt><dd className="mono small">{a.latestSnapshot}</dd></>}
      </dl>
    </section>
  )
}

// ZoneSummary: the zone's status bar, expanded. Same counts as the bar, then
// everything running in the zone by kind, with what the map folds away.
function ZoneSummary({ lab, zone, onOpenNode }: { lab: Lab; zone: LabNode; onOpenNode: (id: string) => void }) {
  const st = zone.summary!.stats as { agents: number; workloads: number; ready: number; pods: number; rps: number; err: number; idle: number; down: number }
  const idle = new Set(zone.summary!.idle as string[])
  const members = (zone.summary!.members as string[]).map(id => lab.graph?.nodes.find(n => n.id === id)).filter(Boolean) as LabNode[]
  const kinds = [...new Set(members.map(n => n.kind))].sort((a, b) => (kindLabel[a] ?? a).localeCompare(kindLabel[b] ?? b))
  return (
    <section>
      <div className="zstats">
        {st.agents > 0 && <div><b>{st.agents}</b><span>agents</span></div>}
        <div><b>{st.workloads}</b><span>workloads</span></div>
        <div className={st.ready < st.pods ? 'warn' : ''}><b>{st.ready}/{st.pods}</b><span>pods ready</span></div>
        <div><b>{st.rps >= 10 ? st.rps.toFixed(0) : st.rps.toFixed(1)}</b><span>req/s</span></div>
        <div className={st.err > 0 ? 'bad' : ''}><b>{(st.err * 100).toFixed(1)}%</b><span>errors</span></div>
      </div>
      {kinds.map(k => (
        <div key={k} className="zkind">
          <div className="label">{kindLabel[k] ?? k} · {members.filter(n => n.kind === k).length}</div>
          {members.filter(n => n.kind === k).sort((a, b) => a.label.localeCompare(b.label)).map(n => {
            const pods = n.pods ?? []
            return (
              <button key={n.id} className="conn" onClick={() => onOpenNode(n.id)} title={n.namespace}>
                <span className={`dot ${n.status}`} />
                <span className="grow ellipsis">{n.label}</span>
                {idle.has(n.id) && <span className="chip">idle</span>}
                {pods.length > 0 && <span className="subtle mono">{pods.filter(p => p.ready).length}/{pods.length} pods</span>}
              </button>
            )
          })}
        </div>
      ))}
    </section>
  )
}
