import { lazy, Suspense, useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { addRelationship, evaluateRelationships, getRelationships, isConflict, removeRelationship, saveRelationshipModel,
  type CodeMarker, type GuardDecision, type RelationshipItem, type RelationshipModelPhase, type RelationshipsView } from '../api'
import { Guard } from '../topology/DetailsPanel'

const CodeEditor = lazy(() => import('./CodeEditor'))

type Tab = 'model' | 'relationships' | 'whatif'
const TABS: [Tab, string][] = [['model', 'Model'], ['relationships', 'Relationships'], ['whatif', 'What if']]
// object types listed first, in this order; any other after them
const ORDER = ['agent', 'mcp_server', 'book', 'client']
const typeOf = (o: string) => o.slice(0, o.indexOf(':'))
const idOf = (o: string) => o.slice(o.indexOf(':') + 1)
const rank = (t: string) => { const i = ORDER.indexOf(t); return i < 0 ? ORDER.length : i }

// Relationships: the entitlements the gateway checks on every tool call, as
// a RelationshipModel (OpenFGA's DSL) and the Relationships naming it. The
// continuity controller keeps OpenFGA matching them. Saving the model, or
// adding or removing a relationship, writes it as the signed-in admin; the
// what-if is the relationship guard's own answer.
export function Relationships({ models, onClose }: { models: RelationshipModelPhase[]; onClose: () => void }) {
  const [pick, setPick] = useState(`${models[0].namespace}/${models[0].name}`)
  const m = models.find(x => `${x.namespace}/${x.name}` === pick) ?? models[0]
  const ns = m.namespace, name = m.name
  const [view, setView] = useState<RelationshipsView>()
  const [tab, setTab] = useState<Tab>('model')
  const [msg, setMsg] = useState<{ text: string; conflict?: boolean }>()
  const [busy, setBusy] = useState(false)
  const [draft, setDraft] = useState({ user: '', relation: '', object: '' })
  const [reads, setReads] = useState(0)
  // the model as edited (undefined: the live one), and where OpenFGA's parser stops
  const [code, setCode] = useState<string>()
  const [codeErrs, setCodeErrs] = useState<CodeMarker[]>()
  const [saved, setSaved] = useState<string>()
  const dialog = useRef<HTMLDivElement>(null)
  const dirty = code !== undefined && code !== view?.model

  useEffect(() => {
    let live = true
    getRelationships(ns, name).then(v => { if (live) setView(v) }, e => { if (live) setMsg({ text: (e as Error).message }) })
    return () => { live = false }
  }, [ns, name, reads])
  const reload = () => { setMsg(undefined); setReads(n => n + 1) }
  const revert = () => { setCode(undefined); setCodeErrs(undefined); setMsg(undefined); setSaved(undefined) }

  // after a write: the view once it shows it (the server reads a watch
  // cache, a moment behind its own writes)
  const settle = async (done: (v: RelationshipsView) => boolean) => {
    for (let i = 0; ; i++) {
      const v = await getRelationships(ns, name)
      if (done(v) || i >= 20) { setView(v); return }
      await new Promise(r => setTimeout(r, 250))
    }
  }
  const write = async (f: () => Promise<unknown>) => {
    setBusy(true); setMsg(undefined)
    try { await f() } catch (e) { setMsg({ text: (e as Error).message, conflict: isConflict(e) }) } finally { setBusy(false) }
  }
  const add = () => write(async () => {
    const r = await addRelationship(ns, name, draft)
    setDraft({ user: '', relation: '', object: '' })
    await settle(v => v.relationships.some(x => x.name === r.name))
  })
  const saveModel = () => write(async () => {
    if (!view || code === undefined) return
    setSaved(undefined)
    const r = await saveRelationshipModel(ns, name, code, view.resourceVersion)
    if ('errors' in r) { setCodeErrs(r.errors); return }
    setCodeErrs(undefined)
    setSaved('Saved. The continuity controller writes it to OpenFGA; the status above says when it has.')
    await settle(v => v.resourceVersion === r.resourceVersion)
    setCode(undefined)
    setTimeout(reload, 4000)
  })
  const remove = (r: RelationshipItem) => write(async () => {
    await removeRelationship(ns, name, r)
    await settle(v => !v.relationships.some(x => x.name === r.name))
  })

  const close = () => {
    if (dirty && !window.confirm('Discard the unsaved model?')) return
    onClose()
  }
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && !e.defaultPrevented) close() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  })
  useEffect(() => {
    const root = document.getElementById('root')
    const prev = document.activeElement as HTMLElement | null
    root?.setAttribute('inert', '')
    dialog.current?.focus()
    return () => { root?.removeAttribute('inert'); prev?.focus() }
  }, [])
  const onTabKey = (e: ReactKeyboardEvent) => {
    const i = TABS.findIndex(([t]) => t === tab)
    const j = e.key === 'ArrowRight' ? (i + 1) % TABS.length : e.key === 'ArrowLeft' ? (i + TABS.length - 1) % TABS.length : -1
    if (j < 0) return
    e.preventDefault()
    setTab(TABS[j][0])
    document.getElementById(`rel-tab-${TABS[j][0]}`)?.focus()
  }

  const groups = useMemo(() => {
    const g = new Map<string, RelationshipItem[]>()
    for (const r of view?.relationships ?? []) g.set(typeOf(r.object), [...(g.get(typeOf(r.object)) ?? []), r])
    for (const rs of g.values()) rs.sort((a, b) => a.object.localeCompare(b.object) || a.relation.localeCompare(b.relation) || a.user.localeCompare(b.user))
    return [...g].sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b))
  }, [view])
  const synced = view?.status?.conditions?.find(c => c.type === 'Synced')

  return createPortal(
    <div className="cm-overlay" onMouseDown={e => { if (e.target === e.currentTarget) close() }}>
      <div className="cm-window" role="dialog" aria-modal="true" aria-labelledby="rel-title" tabIndex={-1} ref={dialog}>
        <header className="cm-header">
          <h2 id="rel-title">Relationships</h2>
          {models.length > 1
            ? (
              <select className="field" aria-label="RelationshipModel" value={pick} onChange={e => { setPick(e.target.value); setView(undefined); setMsg(undefined) }}>
                {models.map(x => <option key={`${x.namespace}/${x.name}`} value={`${x.namespace}/${x.name}`}>{x.namespace}/{x.name}</option>)}
              </select>
            )
            : <span className="mono subtle">{ns}/{name}</span>}
          <div className="tabs" role="tablist" aria-label="Relationships views" onKeyDown={onTabKey}>
            {TABS.map(([t, label]) => (
              <button key={t} id={`rel-tab-${t}`} role="tab" aria-selected={tab === t} tabIndex={tab === t ? 0 : -1}
                className={tab === t ? 'tab active' : 'tab'} onClick={() => setTab(t)}>{label}</button>
            ))}
          </div>
          <span className="grow" />
          {view?.guard && <span className="subtle small mono" title="the gateway's relationship guard, which checks these on every tool call">guard {view.guard.namespace}/{view.guard.name}</span>}
          {tab === 'model' && <>
            {dirty && <span className="chip warn">unsaved</span>}
            <button className="btn small ghost" disabled={!dirty || busy} onClick={revert}>Revert</button>
            <button className="btn small primary" disabled={!dirty || busy} onClick={saveModel}>Save</button>
          </>}
          <button className="btn small" onClick={close} title="Close (Esc)">Close</button>
        </header>
        {msg && tab !== 'relationships' && (
          <div className="note bad cm-msg">
            {msg.text}
            {msg.conflict && <> <button className="btn small" onClick={() => { revert(); reload() }}>Reload</button></>}
          </div>
        )}
        {saved && tab === 'model' && <div className="note ok cm-msg">{saved}</div>}
        {view && (
          <div className="cm-body">
            {tab === 'model' && (
              <div className="cm-code">
                {view.description && <p className="subtle small">{view.description}</p>}
                <div className="row wrap small">
                  {synced?.status === 'True' ? <span className="chip ok">Synced</span> : <span className="chip bad">{synced?.message || 'not synced yet'}</span>}
                  {view.status?.storeID && <span className="subtle">store <span className="mono">{view.status.storeID}</span></span>}
                  {view.status?.modelID && <span className="subtle">model <span className="mono">{view.status.modelID}</span></span>}
                  <span className="subtle">{view.status?.relationships ?? 0} relationships in OpenFGA</span>
                </div>
                <div className="monaco">
                  <Guard>
                    <Suspense fallback={<div className="subtle small">Loading editor…</div>}>
                      <CodeEditor value={code ?? view.model} onChange={v => { setCode(v); if (codeErrs) setCodeErrs(undefined) }} language="openfga" markers={codeErrs} />
                    </Suspense>
                  </Guard>
                </div>
                {codeErrs && (
                  <ul className="note bad small" role="alert">
                    {codeErrs.map((e, i) => <li key={i}><span className="mono">{e.line}:{e.col}</span> {e.message}</li>)}
                  </ul>
                )}
              </div>
            )}
            {tab === 'relationships' && (
              <div className="rel-list scroll">
                <form className="rel-grid rel-add" onSubmit={e => { e.preventDefault(); add() }}>
                  <input className="field mono" aria-label="user" value={draft.user} placeholder="user: type:id or type:id#relation" onChange={e => setDraft({ ...draft, user: e.target.value.trim() })} />
                  <input className="field mono" aria-label="relation" value={draft.relation} placeholder="relation" onChange={e => setDraft({ ...draft, relation: e.target.value.trim() })} />
                  <input className="field mono" aria-label="object" value={draft.object} placeholder="object: type:id" onChange={e => setDraft({ ...draft, object: e.target.value.trim() })} />
                  <button className="btn small primary" disabled={busy || !draft.user || !draft.relation || !draft.object}>Add</button>
                </form>
                {msg && (
                  <div className="note bad small" role="alert">
                    {msg.text}
                    {msg.conflict && <> <button className="btn small" onClick={reload}>Reload</button></>}
                  </div>
                )}
                {groups.map(([type, rs]) => (
                  <section key={type}>
                    <div className="ar-group">{type}</div>
                    <ul className="rel-rows">
                      {rs.map(r => (
                        <li key={r.name} className="rel-grid" title={r.name}>
                          <span className="mono ellipsis">{r.user}</span>
                          <span className="mono subtle">{r.relation}</span>
                          <span className="mono ellipsis">{r.object}</span>
                          <button className="cm-x" disabled={busy} aria-label={`Remove ${r.user} ${r.relation} ${r.object}`} onClick={() => remove(r)}>×</button>
                        </li>
                      ))}
                    </ul>
                  </section>
                ))}
                {!groups.length && <div className="subtle small">No relationships name this model yet.</div>}
              </div>
            )}
            {tab === 'whatif' && <WhatIf key={pick} view={view} run={q => evaluateRelationships(ns, name, q)} />}
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

// WhatIf: one caller (a user, their groups as the IdP would assert them, the
// agent acting for them) calling one MCP server, as the guard decides it.
function WhatIf({ view, run }: {
  view: RelationshipsView; run: (q: { user: string; groups: string[]; agent: string; server: string }) => Promise<GuardDecision>
}) {
  const objects = (type: string) => [...new Set(view.relationships.map(r => r.object).filter(o => typeOf(o) === type))].sort()
  const agents = objects('agent'), servers = objects('mcp_server').map(idOf)
  const [user, setUser] = useState(() => idOf(view.relationships.map(r => r.user).find(u => typeOf(u) === 'user' && !u.includes('#')) ?? ''))
  // the groups the relationships name (group:<id>#member), as the token would carry them
  const [groups, setGroups] = useState(() => [...new Set(view.relationships.map(r => r.user).filter(u => typeOf(u) === 'group').map(u => idOf(u).split('#')[0]))].sort().join(', '))
  const [agent, setAgent] = useState(agents[0] ?? '')
  const [server, setServer] = useState(servers[0] ?? '')
  const [out, setOut] = useState<GuardDecision>()
  const [err, setErr] = useState<string>()
  const [busy, setBusy] = useState(false)
  const go = async () => {
    setBusy(true); setErr(undefined)
    try { setOut(await run({ user, groups: groups.split(',').map(g => g.trim()).filter(Boolean), agent, server })) }
    catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }
  const mark = (ok: boolean) => <span className={ok ? 'ok-text' : 'danger-text'}>{ok ? '✓' : '✗'}</span>
  return (
    <div className="ar-whatif scroll">
      {!view.guard && <div className="note bad">No relationship guard: a Service labelled continuity.lab.solo.io/relationship-guard with a port named evaluate.</div>}
      <div className="ar-whatif-form">
        <label>user email
          <input className="field mono" value={user} onChange={e => setUser(e.target.value.trim())} placeholder="user email" />
        </label>
        <label>groups
          <input className="field mono" value={groups} onChange={e => setGroups(e.target.value)} placeholder="comma-separated" />
        </label>
        <label>agent
          <input className="field mono" list="rel-agents" value={agent} onChange={e => setAgent(e.target.value.trim())} placeholder="agent:namespace.serviceaccount" />
          <datalist id="rel-agents">{agents.map(a => <option key={a} value={a} />)}</datalist>
        </label>
        <label>MCP server
          <select className="field" value={server} onChange={e => setServer(e.target.value)}>
            {servers.map(s => <option key={s} value={s}>{s}</option>)}
          </select>
        </label>
        <button className="btn primary" disabled={busy || !view.guard || !user || !agent || !server} onClick={go}>Evaluate</button>
      </div>
      {err && <div className="note bad">{err}</div>}
      {out && (
        <dl className="ar-facts">
          <dt>Decision</dt><dd><span className={out.decision.allow ? 'chip ok' : 'chip bad'}>{out.decision.allow ? 'allow' : 'deny'}</span></dd>
          <dt>Acts for</dt><dd>{mark(out.decision.actsFor)} <span className="mono subtle">{out.agent}</span></dd>
          <dt>May call</dt><dd>{mark(out.decision.canCall)} <span className="mono subtle">{server}</span></dd>
          <dt>Clients</dt><dd className="mono">{out.decision.clients.join(', ') || <span className="subtle">none</span>}</dd>
          <dt>Reason</dt><dd>{out.decision.reason}</dd>
        </dl>
      )}
    </div>
  )
}
