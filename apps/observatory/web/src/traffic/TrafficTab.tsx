import { useMemo, useState } from 'react'
import type { Lab } from '../api'
import { TrafficRow } from './TrafficRow'
import './traffic.css'

const KINDS = ['http', 'mcp', 'a2a', 'llm', 'oidc', 'substrate', 'lifecycle', 'continuity', 'model']
const OUTCOMES = ['ok', 'denied', 'error', 'info']

// TrafficTab: every request and runtime event in the lab, newest first.
export function TrafficTab({ lab, onOpenNode }: { lab: Lab; onOpenNode: (id: string) => void }) {
  const [kinds, setKinds] = useState<Set<string>>(new Set(KINDS))
  const [outcomes, setOutcomes] = useState<Set<string>>(new Set(OUTCOMES))
  const [q, setQ] = useState('')
  const [paused, setPaused] = useState<typeof lab.traffic>()
  const [hideDiscovery, setHideDiscovery] = useState(true)
  const [withToken, setWithToken] = useState(false)
  const names = useMemo(() => new Map(lab.graph?.nodes.map(n => [n.id, n.label]) ?? []), [lab.graph])

  const rows = useMemo(() => {
    const src = paused ?? lab.traffic
    const needle = q.trim().toLowerCase()
    return src.filter(t => {
      if (!kinds.has(t.kind) || !outcomes.has(t.outcome)) return false
      if (withToken && !t.tokens?.length) return false
      // MCP session setup and tool listing: clients poll it; real, but noise
      if (hideDiscovery && t.kind === 'mcp' && /^(tools\/list|initialize|notifications\/initialized|)$/.test(t.attrs?.['mcp.method.name'] ?? '')) return false
      if (!needle) return true
      return `${t.summary} ${t.user ?? ''} ${t.identity ?? ''} ${names.get(t.source ?? '') ?? ''} ${names.get(t.target ?? '') ?? ''} ${names.get(t.via ?? '') ?? ''} ${t.path ?? ''}`
        .toLowerCase().includes(needle)
    }).slice(0, 800)
  }, [lab.traffic, paused, kinds, outcomes, q, names, hideDiscovery, withToken])

  const flip = (s: Set<string>, v: string, set: (s: Set<string>) => void) => {
    const n = new Set(s)
    if (n.has(v)) n.delete(v); else n.add(v)
    set(n)
  }

  return (
    <div className="traffic">
      <div className="tbar">
        <input className="field" placeholder="Filter by user, workload, tool, path…" aria-label="Filter traffic" value={q} onChange={e => setQ(e.target.value)} />
        <div className="seg">
          {KINDS.map(k => <button key={k} className={kinds.has(k) ? 'on' : ''} onClick={() => flip(kinds, k, setKinds)}>{k}</button>)}
        </div>
        <div className="seg">
          {OUTCOMES.map(k => <button key={k} className={outcomes.has(k) ? 'on' : ''} onClick={() => flip(outcomes, k, setOutcomes)}>{k}</button>)}
        </div>
        <button className={hideDiscovery ? 'btn small' : 'btn small ghost'} title="MCP initialize and tools/list calls" onClick={() => setHideDiscovery(v => !v)}>Hide tool discovery</button>
        <button className={withToken ? 'btn small primary' : 'btn small'} title="only requests that carried a token (expand one to see its claims)" onClick={() => setWithToken(v => !v)}>Carries a token</button>
        <button className={paused ? 'btn small primary' : 'btn small'} onClick={() => setPaused(paused ? undefined : lab.traffic)}>{paused ? 'Resume' : 'Pause'}</button>
        <span className="subtle small">{rows.length} shown</span>
      </div>
      <div className="thead">
        <span className="ttime">time</span><span /><span className="tk">kind</span><span className="grow">request</span><span className="tpath">path</span><span>status</span><span className="tdur">duration</span>
      </div>
      <div className="tscroll scroll">
        {rows.length === 0 && <div className="empty-list subtle">No traffic matches. Drive a demo and it streams in here.</div>}
        {rows.map(t => <TrafficRow key={t.id} t={t} names={names} onOpenNode={onOpenNode} />)}
      </div>
    </div>
  )
}
